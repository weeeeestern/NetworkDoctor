package investigate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"networkdoctor-agent/internal/backend/check"
	"networkdoctor-agent/internal/backend/holmes"
	"networkdoctor-agent/internal/backend/incident"
)

// Arch is an investigation architecture. All three share evidence
// collection, the Holmes call and the parse retry; they differ only in what
// is added around Holmes, so a comparison isolates that addition.
type Arch string

const (
	// ArchBaseline: evidence snapshot + Holmes (the original pipeline).
	ArchBaseline Arch = "baseline"
	// ArchDerived: baseline + deterministic scenario facts in the request.
	ArchDerived Arch = "derived"
	// ArchDerivedJev: derived + a Jev consistency check of the conclusion,
	// with one Holmes re-examination when the check fails.
	ArchDerivedJev Arch = "derived+jev"
)

// ParseArch validates an architecture name ("" = baseline).
func ParseArch(s string) (Arch, error) {
	switch Arch(s) {
	case "", ArchBaseline:
		return ArchBaseline, nil
	case ArchDerived, ArchDerivedJev:
		return Arch(s), nil
	}
	return "", fmt.Errorf("unknown arch %q (want baseline, derived or derived+jev)", s)
}

// CheckThreshold is the minimum consistency probability that passes.
const CheckThreshold = 0.5

// Outcome is the full record of one investigation run.
type Outcome struct {
	Arch        Arch                `json:"arch"`
	Model       string              `json:"model,omitempty"`
	WindowStart time.Time           `json:"window_start"`
	WindowEnd   time.Time           `json:"window_end"`
	Evidence    []incident.Evidence `json:"evidence,omitempty"`
	Derived     []incident.Evidence `json:"derived_facts,omitempty"`
	Analysis    string              `json:"analysis,omitempty"`
	Result      map[string]any      `json:"result,omitempty"`
	ToolCalls   int                 `json:"tool_calls"`
	HolmesCalls int                 `json:"holmes_calls"`
	Checks      []check.Verdict     `json:"checks,omitempty"`
	// Rechecked is true when a failed check sent Holmes back once.
	Rechecked  bool   `json:"rechecked,omitempty"`
	CheckError string `json:"check_error,omitempty"`
	ParseError string `json:"parse_error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"` // done | failed
	Error      string `json:"error,omitempty"`
}

// Investigate runs one investigation of inc with the given architecture and
// returns the outcome without touching the store.
func (r *Runner) Investigate(ctx context.Context, inc *incident.Incident, related []*incident.Incident, arch Arch) Outcome {
	return r.investigate(ctx, inc, related, arch, r.o.Holmes, r.o.Model)
}

// investigate is Investigate with an explicit Holmes client, so evaluation
// runs can use their own model entry (and gateway key).
func (r *Runner) investigate(ctx context.Context, inc *incident.Incident, related []*incident.Incident, arch Arch, h Asker, model string) (out Outcome) {
	t0 := time.Now()
	now := r.o.Now().UTC()
	out = Outcome{Arch: arch, Model: model, Status: "failed"}
	out.WindowStart = inc.StartsAt.Add(-r.o.Window)
	out.WindowEnd = inc.StartsAt.Add(r.o.Window)
	if out.WindowEnd.After(now) {
		out.WindowEnd = now
	}
	// Named result: the deferred assignment must reach the returned value.
	defer func() { out.DurationMS = time.Since(t0).Milliseconds() }()

	// 1. Evidence snapshot (all architectures).
	if r.o.Evidence != nil {
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		out.Evidence = r.o.Evidence.Collect(ectx, firstOr(inc.AffectedNodes, ""), out.WindowStart, out.WindowEnd)
		cancel()
	}
	// 2. Derived facts (derived architectures).
	if arch != ArchBaseline && r.o.Deriver != nil {
		dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		out.Derived = r.o.Deriver.Derive(dctx, inc, related, out.WindowStart, out.WindowEnd)
		cancel()
	}

	if h == nil {
		out.Error = "holmes is not configured"
		return out
	}
	hctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()

	withEv := *inc
	withEv.EvidenceMetrics = out.Evidence
	ask := holmes.BuildAskWithFacts(&withEv, related, out.WindowStart, out.WindowEnd, out.Derived)

	// 3. Holmes, with one follow-up when the answer has no parseable result.
	ans, result, err := r.askParsed(hctx, h, ask, &out)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Analysis, out.Result = ans.Analysis, result

	// 4. Conclusion check (derived+jev).
	if arch == ArchDerivedJev && result != nil {
		if r.o.Checker == nil {
			out.CheckError = "checker not configured (JEV_API_KEY unset); ran as derived"
		} else {
			in := check.Input{Labels: inc.AlertLabels, Derived: out.Derived, Result: result}
			v, cerr := r.o.Checker.Check(hctx, in)
			if cerr != nil {
				// Fail-open on the investigation path: keep Holmes' answer.
				out.CheckError = cerr.Error()
			} else {
				out.Checks = append(out.Checks, v)
				if !v.Pass(CheckThreshold) {
					out.Rechecked = true
					ans2, res2, err2 := r.askParsed(hctx, h, ask+check.Feedback(in, v), &out)
					if err2 == nil && res2 != nil {
						out.Analysis, out.Result = ans2.Analysis, res2
						if v2, e := r.o.Checker.Check(hctx, check.Input{Labels: inc.AlertLabels, Derived: out.Derived, Result: res2}); e == nil {
							out.Checks = append(out.Checks, v2)
						}
					}
				}
			}
		}
	}

	if out.Result == nil {
		out.Error = "answer received but not parseable: " + out.ParseError
		return out
	}
	out.Status = "done"
	return out
}

// askParsed asks Holmes and, if the answer has no result block, asks once
// more. Tool calls and Holmes calls accumulate on out.
func (r *Runner) askParsed(ctx context.Context, h Asker, ask string, out *Outcome) (holmes.Answer, map[string]any, error) {
	ans, err := h.Ask(ctx, ask)
	out.HolmesCalls++
	if err != nil {
		return holmes.Answer{}, nil, err
	}
	out.ToolCalls += ans.ToolCalls
	result, perr := holmes.ParseResult(ans.Analysis)
	if perr != nil {
		r.o.Logger.Printf("answer not parseable (%v, %d tool calls); asking once more", perr, ans.ToolCalls)
		ans2, err2 := h.Ask(ctx, holmes.RetryAsk(ask, ans.Analysis))
		out.HolmesCalls++
		if err2 == nil {
			out.ToolCalls += ans2.ToolCalls
			ans = ans2
			result, perr = holmes.ParseResult(ans.Analysis)
		}
	}
	if perr != nil {
		out.ParseError = perr.Error()
		return ans, nil, nil
	}
	out.ParseError = ""
	return ans, result, nil
}

// ------------------------------------------------------------------ eval

// EvalRun is the stored record of one evaluation run.
type EvalRun struct {
	RunID      string    `json:"run_id"`
	IncidentID string    `json:"incident_id"`
	Arch       Arch      `json:"arch"`
	Status     string    `json:"status"` // queued | running | done | failed
	CreatedAt  time.Time `json:"created_at"`
	Outcome    *Outcome  `json:"outcome,omitempty"`
}

var runIDRe = regexp.MustCompile(`^ev-[0-9a-f]{12}$`)

// ErrEvalDisabled is returned when no eval directory is configured.
var ErrEvalDisabled = errors.New("evaluation runs are disabled (no eval dir)")

// StartEval re-investigates an existing incident with the given architecture
// in the background and returns a run id. The incident itself is not changed:
// the outcome is written to <EvalDir>/<run id>.json.
func (r *Runner) StartEval(incidentID, archName string) (string, error) {
	if r.o.EvalDir == "" {
		return "", ErrEvalDisabled
	}
	arch, err := ParseArch(archName)
	if err != nil {
		return "", err
	}
	inc, err := r.o.Store.Get(incidentID)
	if err != nil {
		return "", err
	}
	related, _ := r.o.Store.Members(inc.IncidentID)

	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	run := &EvalRun{RunID: "ev-" + hex.EncodeToString(buf), IncidentID: inc.IncidentID, Arch: arch,
		Status: "queued", CreatedAt: r.o.Now().UTC()}
	if err := r.saveEval(run); err != nil {
		return "", err
	}
	go func() {
		r.evalSem <- struct{}{}
		defer func() { <-r.evalSem }()
		run.Status = "running"
		_ = r.saveEval(run)
		h, model := r.o.Holmes, r.o.Model
		if r.o.EvalHolmes != nil {
			h, model = r.o.EvalHolmes, r.o.EvalModel
		}
		o := r.investigate(r.ctx, inc, related, arch, h, model)
		run.Outcome, run.Status = &o, o.Status
		if err := r.saveEval(run); err != nil {
			r.o.Logger.Printf("eval %s: save: %v", run.RunID, err)
		}
		r.o.Logger.Printf("eval %s: %s on %s -> %s in %dms (%d tool calls, %d holmes calls)",
			run.RunID, arch, inc.IncidentID, o.Status, o.DurationMS, o.ToolCalls, o.HolmesCalls)
	}()
	return run.RunID, nil
}

// EvalResult returns the stored JSON of a run.
func (r *Runner) EvalResult(runID string) ([]byte, error) {
	if r.o.EvalDir == "" {
		return nil, ErrEvalDisabled
	}
	if !runIDRe.MatchString(runID) {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(r.o.EvalDir, runID+".json"))
}

func (r *Runner) saveEval(run *EvalRun) error {
	if err := os.MkdirAll(r.o.EvalDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(r.o.EvalDir, run.RunID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.o.EvalDir, run.RunID+".json"))
}
