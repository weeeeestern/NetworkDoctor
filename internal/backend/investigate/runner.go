// Package investigate runs the follow-up work for an incident in the
// background: collect PromQL evidence, ask HolmesGPT once, store the result.
//
// Once-per-incident: an automatic run happens only while holmes_attempts is
// 0. A human retry (POST /incidents/{id}/holmes) sets force and runs again.
package investigate

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/evidence"
	"networkdoctor-agent/internal/backend/holmes"
	"networkdoctor-agent/internal/backend/incident"
)

// Store is the subset of incident.FileStore the runner needs.
type Store interface {
	Get(id string) (*incident.Incident, error)
	Update(id string, mutate func(*incident.Incident) error) (*incident.Incident, error)
	Members(primaryID string) ([]*incident.Incident, error)
}

// Asker is the subset of holmes.Client the runner needs.
type Asker interface {
	Ask(ctx context.Context, ask string) (holmes.Answer, error)
}

// Options configures a Runner.
type Options struct {
	Store        Store
	Evidence     *evidence.Collector // nil disables evidence collection
	Holmes       Asker               // nil disables Holmes
	Model        string
	Window       time.Duration
	Timeout      time.Duration
	SkipPrefixes []string
	Workers      int
	// GroupWait delays a primary's investigation so alerts that fire
	// together can join its correlation group first.
	GroupWait time.Duration
	Logger    *log.Logger
	Now       func() time.Time
}

type job struct {
	id    string
	force bool
}

// Runner is a small bounded work queue.
type Runner struct {
	o     Options
	queue chan job
}

// New starts the workers. They stop when ctx is cancelled.
func New(ctx context.Context, o Options) *Runner {
	if o.Workers <= 0 {
		o.Workers = 1
	}
	if o.Window <= 0 {
		o.Window = 15 * time.Minute
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	r := &Runner{o: o, queue: make(chan job, 64)}
	for i := 0; i < o.Workers; i++ {
		go r.loop(ctx)
	}
	return r
}

// ParseSkipPrefixes splits a comma-separated prefix list.
func ParseSkipPrefixes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ErrQueueFull is returned when the queue cannot accept more work.
var ErrQueueFull = errors.New("investigation queue is full")

// Enqueue schedules an investigation. It never blocks.
func (r *Runner) Enqueue(id string, force bool) error {
	select {
	case r.queue <- job{id: id, force: force}:
		return nil
	default:
		return ErrQueueFull
	}
}

func (r *Runner) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-r.queue:
			r.run(ctx, j)
		}
	}
}

var errSkip = errors.New("skip")

func (r *Runner) run(ctx context.Context, j job) {
	// Group members are investigated with their primary, not on their own.
	if !j.force {
		cur, err := r.o.Store.Get(j.id)
		if err != nil {
			r.o.Logger.Printf("investigate %s: %v", j.id, err)
			return
		}
		if cur.IsGroupMember() {
			r.markGrouped(cur.IncidentID, cur.CorrelationID)
			return
		}
		if r.o.GroupWait > 0 && !r.skipped(cur.RuleID) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.o.GroupWait):
			}
		}
	}
	now := r.o.Now().UTC()

	// Claim the incident: decide under the store lock whether to run.
	inc, err := r.o.Store.Update(j.id, func(inc *incident.Incident) error {
		if !j.force {
			if inc.HolmesAttempts > 0 || inc.HolmesStatus == "running" {
				return incident.ErrNoChange
			}
			if r.skipped(inc.RuleID) {
				inc.HolmesStatus = "skipped"
				inc.HolmesError = "rule_id matches an excluded prefix (" + strings.Join(r.o.SkipPrefixes, ",") + ")"
				return nil
			}
		}
		inc.HolmesStatus = "running"
		inc.HolmesError = ""
		inc.HolmesAttempts++
		inc.HolmesModel = r.o.Model
		inc.HolmesLastAt = &now
		return nil
	})
	if err != nil {
		r.o.Logger.Printf("investigate %s: claim failed: %v", j.id, err)
		return
	}
	if inc.HolmesStatus != "running" {
		return
	}

	var related []*incident.Incident
	if !j.force || !inc.IsGroupMember() {
		if ms, err := r.o.Store.Members(inc.IncidentID); err == nil {
			related = ms
			for _, m := range ms {
				r.markGrouped(m.IncidentID, inc.IncidentID)
			}
		}
	}

	start := inc.StartsAt.Add(-r.o.Window)
	end := inc.StartsAt.Add(r.o.Window)
	if end.After(now) {
		end = now
	}

	// 1. Evidence snapshot.
	if r.o.Evidence != nil {
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		node := firstOr(inc.AffectedNodes, "")
		ev := r.o.Evidence.Collect(ectx, node, start, end)
		cancel()
		inc, err = r.o.Store.Update(j.id, func(i *incident.Incident) error {
			i.EvidenceMetrics = ev
			i.UpdatedAt = r.o.Now().UTC()
			return nil
		})
		if err != nil {
			r.o.Logger.Printf("investigate %s: save evidence: %v", j.id, err)
			return
		}
		r.o.Logger.Printf("investigate %s: collected %d evidence queries", j.id, len(ev))
	}

	// 2. Holmes.
	if r.o.Holmes == nil {
		r.finish(j.id, "failed", "holmes is not configured", holmes.Answer{}, nil)
		return
	}
	hctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	t0 := time.Now()
	ask := holmes.BuildAsk(inc, related, start, end)
	ans, err := r.o.Holmes.Ask(hctx, ask)
	if err != nil {
		r.finish(j.id, "failed", err.Error(), holmes.Answer{}, nil)
		r.o.Logger.Printf("investigate %s: holmes failed after %s: %v", j.id, time.Since(t0).Round(time.Second), err)
		return
	}
	result, perr := holmes.ParseResult(ans.Analysis)
	if perr != nil {
		// One automatic follow-up: some models end the turn after stating a
		// plan. Same incident, same claim, so this does not break once-per-incident.
		r.o.Logger.Printf("investigate %s: answer not parseable (%v, %d tool calls); asking once more", j.id, perr, ans.ToolCalls)
		ans2, err2 := r.o.Holmes.Ask(hctx, holmes.RetryAsk(ask, ans.Analysis))
		if err2 == nil {
			ans2.ToolCalls += ans.ToolCalls
			ans = ans2
			result, perr = holmes.ParseResult(ans.Analysis)
		}
	}
	status, msg := "done", ""
	if perr != nil {
		status, msg = "failed", "answer received but not parseable: "+perr.Error()
	}
	r.finish(j.id, status, msg, ans, result)
	r.o.Logger.Printf("investigate %s: holmes %s in %s (%d tool calls)", j.id, status, time.Since(t0).Round(time.Second), ans.ToolCalls)
}

func (r *Runner) finish(id, status, msg string, ans holmes.Answer, result map[string]any) {
	_, err := r.o.Store.Update(id, func(i *incident.Incident) error {
		i.HolmesStatus = status
		i.HolmesError = msg
		i.HolmesAnalysis = ans.Analysis
		i.HolmesToolCalls = ans.ToolCalls
		i.UpdatedAt = r.o.Now().UTC()
		if result != nil {
			i.HolmesResult = result
			applyResult(i, result)
		}
		return nil
	})
	if err != nil {
		r.o.Logger.Printf("investigate %s: save result: %v", id, err)
	}
}

// applyResult copies the headline fields of the RCA into the incident's own
// fields so list views and reports do not need to dig into holmes_result.
func applyResult(i *incident.Incident, r map[string]any) {
	cause, _ := r["root_cause"].(string)
	conf, _ := r["confidence"].(string)
	st, _ := r["investigation_status"].(string)
	if cause != "" {
		i.RootCauseCandidates = []incident.RootCauseCandidate{{
			Cause: strings.TrimSpace(cause), Confidence: conf, Reason: "HolmesGPT investigation_status=" + st,
		}}
	}
	if acts, ok := r["recommended_actions"].([]any); ok {
		i.RecommendedActions = i.RecommendedActions[:0]
		for _, a := range acts {
			if s, ok := a.(string); ok && s != "" {
				i.RecommendedActions = append(i.RecommendedActions, s)
			}
		}
	}
}

// markGrouped records that an incident is covered by its primary's
// investigation. A finished or running own investigation is left alone.
func (r *Runner) markGrouped(id, primary string) {
	_, err := r.o.Store.Update(id, func(i *incident.Incident) error {
		if i.HolmesStatus == "done" || i.HolmesStatus == "running" || i.HolmesStatus == "grouped" {
			return incident.ErrNoChange
		}
		i.HolmesStatus = "grouped"
		i.HolmesError = "investigated together with " + primary
		i.UpdatedAt = r.o.Now().UTC()
		return nil
	})
	if err != nil {
		r.o.Logger.Printf("investigate %s: mark grouped: %v", id, err)
	}
}

func (r *Runner) skipped(ruleID string) bool {
	for _, p := range r.o.SkipPrefixes {
		if strings.HasPrefix(ruleID, p) {
			return true
		}
	}
	return false
}

func firstOr(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}
