package investigate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"networkdoctor-agent/internal/backend/alertmanager"
	"networkdoctor-agent/internal/backend/check"
	"networkdoctor-agent/internal/backend/evidence"
	"networkdoctor-agent/internal/backend/holmes"
	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/investigate"
	"networkdoctor-agent/internal/backend/prometheus"
	"networkdoctor-agent/internal/backend/report"
)

const holmesAnswer = "Summary: not congestion.\n\n```yaml\nrule_id: \"rule-1\"\nscenario: \"network-congestion\"\ninvestigation_status: \"excluded\"\nconfidence: \"medium\"\nroot_cause: \"Connect failures to a refusing destination on worker01, not link congestion.\"\ntrigger_evidence:\n  - source: \"rate(ebpf_tcp_retransmits_total[2m])\"\n    observation: \"worker01 peaked at 0.28/s at 08:10Z\"\nexcluded_alternatives:\n  - hypothesis: \"NIC saturation\"\n    reason: \"eno1 drops were 0\"\nrecommended_actions:\n  - \"Check which destination refuses connections from worker01\"\n```\n"

func fakeProm(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"node":"worker02-x"},"values":[[1790582400,"0"],[1790582430,"0.01"]]},
			{"metric":{"node":"worker01-x"},"values":[[1790582400,"0.1"],[1790582430,"0.28"],[1790582460,"NaN"]]}]}}`)
	}))
}

func newIncident(t *testing.T, store *incident.FileStore, ruleID string) *incident.Incident {
	t.Helper()
	a := alertmanager.Alert{
		Status:      "firing",
		Fingerprint: "fp-" + ruleID,
		StartsAt:    time.Date(2026, 9, 28, 8, 9, 20, 0, time.UTC),
		Labels:      map[string]string{"alertname": "NetworkDoctorTCPRetransmitBurstHigh", "rule_id": ruleID, "node": "worker01-x", "severity": "warning"},
	}
	inc, _, err := store.Upsert(a.SourceKey(), func(*incident.Incident) (*incident.Incident, error) {
		return incident.FromAlert(a, incident.Meta{Cluster: "lab"}, time.Now()), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func waitStatus(t *testing.T, store *incident.FileStore, id string, want ...string) *incident.Incident {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		inc, _ := store.Get(id)
		for _, w := range want {
			if inc.HolmesStatus == w {
				return inc
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	inc, _ := store.Get(id)
	t.Fatalf("status %q, want one of %v (error=%q)", inc.HolmesStatus, want, inc.HolmesError)
	return nil
}

func TestRunnerCollectsEvidenceAsksHolmesOnceAndParses(t *testing.T) {
	prom := fakeProm(t)
	defer prom.Close()

	var calls atomic.Int32
	var lastAsk atomic.Value
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		lastAsk.Store(req["ask"])
		json.NewEncoder(w).Encode(map[string]any{"analysis": holmesAnswer, "tool_calls": []any{1, 2, 3}})
	}))
	defer hs.Close()

	store, _ := incident.NewFileStore(t.TempDir())
	inc := newIncident(t, store, "rule-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := investigate.New(ctx, investigate.Options{
		Store:    store,
		Evidence: &evidence.Collector{Prom: prometheus.New(prom.URL)},
		Holmes:   holmes.New(hs.URL, "gateway-sonnet", 5*time.Second),
		Model:    "gateway-sonnet",
		Now:      func() time.Time { return time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC) },
	})

	if err := r.Enqueue(inc.IncidentID, false); err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, store, inc.IncidentID, "done", "failed")
	if got.HolmesStatus != "done" {
		t.Fatalf("status=%s error=%s", got.HolmesStatus, got.HolmesError)
	}

	// A second automatic enqueue must not call Holmes again.
	_ = r.Enqueue(inc.IncidentID, false)
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("holmes called %d times, want 1", n)
	}

	if got.HolmesAttempts != 1 || got.HolmesToolCalls != 3 || got.HolmesModel != "gateway-sonnet" {
		t.Errorf("attempts=%d toolcalls=%d model=%s", got.HolmesAttempts, got.HolmesToolCalls, got.HolmesModel)
	}
	if got.HolmesResult["rule_id"] != "rule-1" || got.HolmesResult["investigation_status"] != "excluded" {
		t.Errorf("holmes_result = %v", got.HolmesResult)
	}
	if len(got.RootCauseCandidates) != 1 || !strings.Contains(got.RootCauseCandidates[0].Cause, "refusing destination") {
		t.Errorf("root_cause_candidates = %+v", got.RootCauseCandidates)
	}
	if len(got.RecommendedActions) != 1 {
		t.Errorf("recommended_actions = %v", got.RecommendedActions)
	}
	if len(got.EvidenceMetrics) != len(evidence.DefaultQueries) {
		t.Fatalf("evidence count = %d", len(got.EvidenceMetrics))
	}
	obs := got.EvidenceMetrics[0].Observation
	if !strings.HasPrefix(obs, "worker01 (alert node): peak 0.280") {
		t.Errorf("evidence observation should list the alert node first with its peak: %q", obs)
	}

	ask, _ := lastAsk.Load().(string)
	for _, want := range []string{"rule_id: \"rule-1\"", "investigation_window: 2026-09-28T07:54:20Z", "evidence already collected", "```yaml"} {
		if !strings.Contains(ask, want) {
			t.Errorf("ask is missing %q", want)
		}
	}
	if strings.Contains(ask, "network-congestion") {
		t.Errorf("backend must not name the skill; routing is the skills' job")
	}

	md := report.Markdown(got)
	for _, want := range []string{"# NetworkDoctorTCPRetransmitBurstHigh", "## Root cause", "refusing destination", "### Excluded alternatives", "## Evidence collected by NetworkDoctor"} {
		if !strings.Contains(md, want) {
			t.Errorf("report is missing %q", want)
		}
	}

	// Manual retry (force) runs again.
	_ = r.Enqueue(inc.IncidentID, true)
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("forced retry did not call holmes")
	}
}

func TestRunnerSkipsExcludedRulePrefixes(t *testing.T) {
	var calls atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer hs.Close()

	store, _ := incident.NewFileStore(t.TempDir())
	inc := newIncident(t, store, "smoke-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := investigate.New(ctx, investigate.Options{
		Store:        store,
		Holmes:       holmes.New(hs.URL, "", time.Second),
		SkipPrefixes: investigate.ParseSkipPrefixes("smoke-, test-"),
	})
	_ = r.Enqueue(inc.IncidentID, false)
	got := waitStatus(t, store, inc.IncidentID, "skipped")
	if calls.Load() != 0 || got.HolmesAttempts != 0 {
		t.Fatalf("skipped incident must not call holmes: calls=%d attempts=%d", calls.Load(), got.HolmesAttempts)
	}
}

func TestPlanOnlyAnswerIsRetriedOnce(t *testing.T) {
	var calls atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var req map[string]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if n == 1 {
			json.NewEncoder(w).Encode(map[string]any{"analysis": "I found the network-congestion skill and will use it.", "tool_calls": []any{1, 2}})
			return
		}
		if !strings.Contains(req["ask"], "previous reply ended before the investigation was done") {
			t.Errorf("retry ask should quote the previous reply")
		}
		json.NewEncoder(w).Encode(map[string]any{"analysis": holmesAnswer, "tool_calls": []any{1, 2, 3, 4}})
	}))
	defer hs.Close()
	store, _ := incident.NewFileStore(t.TempDir())
	inc := newIncident(t, store, "rule-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := investigate.New(ctx, investigate.Options{Store: store, Holmes: holmes.New(hs.URL, "", time.Second)})
	_ = r.Enqueue(inc.IncidentID, false)
	got := waitStatus(t, store, inc.IncidentID, "done", "failed")
	if got.HolmesStatus != "done" || calls.Load() != 2 || got.HolmesToolCalls != 6 || got.HolmesAttempts != 1 {
		t.Fatalf("status=%s calls=%d toolcalls=%d attempts=%d", got.HolmesStatus, calls.Load(), got.HolmesToolCalls, got.HolmesAttempts)
	}
}

func TestUnparseableAnswerIsKeptAndMarkedFailed(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"analysis": "I could not find anything."})
	}))
	defer hs.Close()
	store, _ := incident.NewFileStore(t.TempDir())
	inc := newIncident(t, store, "rule-4")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := investigate.New(ctx, investigate.Options{Store: store, Holmes: holmes.New(hs.URL, "", time.Second)})
	_ = r.Enqueue(inc.IncidentID, false)
	got := waitStatus(t, store, inc.IncidentID, "failed")
	if got.HolmesAnalysis != "I could not find anything." || !strings.Contains(got.HolmesError, "no yaml block") {
		t.Fatalf("analysis=%q error=%q", got.HolmesAnalysis, got.HolmesError)
	}
}

func newIncidentAt(t *testing.T, store *incident.FileStore, ruleID, node string, startsAt time.Time) *incident.Incident {
	t.Helper()
	a := alertmanager.Alert{
		Status:      "firing",
		Fingerprint: "fp-" + ruleID + "-" + node,
		StartsAt:    startsAt,
		Labels:      map[string]string{"alertname": "A-" + ruleID, "rule_id": ruleID, "node": node},
	}
	inc, _, err := store.Upsert(a.SourceKey(), func(*incident.Incident) (*incident.Incident, error) {
		return incident.FromAlert(a, incident.Meta{Cluster: "lab"}, time.Now()), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func TestCorrelateGroupsSameNodeWithinWindow(t *testing.T) {
	store, _ := incident.NewFileStore(t.TempDir())
	t0 := time.Date(2026, 10, 1, 4, 24, 9, 0, time.UTC)
	notSmoke := func(i *incident.Incident) bool { return !strings.HasPrefix(i.RuleID, "smoke-") }

	smoke := newIncidentAt(t, store, "smoke-2", "worker01", t0.Add(-time.Minute))
	r3 := newIncidentAt(t, store, "rule-3", "worker01", t0)
	r5 := newIncidentAt(t, store, "rule-5", "worker01", t0.Add(30*time.Second))
	other := newIncidentAt(t, store, "rule-6", "worker02", t0.Add(time.Minute))
	late := newIncidentAt(t, store, "rule-8", "worker01", t0.Add(20*time.Minute))

	for _, i := range []*incident.Incident{smoke, r3, r5, other, late} {
		if _, err := store.Correlate(i.IncidentID, 10*time.Minute, notSmoke); err != nil {
			t.Fatal(err)
		}
	}
	get := func(i *incident.Incident) *incident.Incident { g, _ := store.Get(i.IncidentID); return g }

	if g := get(smoke); g.IsGroupMember() || g.CorrelationID != smoke.IncidentID {
		t.Errorf("smoke incident must stay alone: %+v", g.CorrelationID)
	}
	if g := get(r3); g.IsGroupMember() || len(g.CorrelatedIncidents) != 1 || g.CorrelatedIncidents[0] != r5.IncidentID {
		t.Errorf("rule-3 should be primary of rule-5: %+v", g.CorrelatedIncidents)
	}
	if g := get(r5); g.CorrelationID != r3.IncidentID {
		t.Errorf("rule-5 should join rule-3, got %s", g.CorrelationID)
	}
	if g := get(other); g.IsGroupMember() {
		t.Errorf("different node must not join")
	}
	if g := get(late); g.IsGroupMember() {
		t.Errorf("incident outside the window must not join")
	}
}

func TestGroupIsInvestigatedOnceWithRelatedAlerts(t *testing.T) {
	var calls atomic.Int32
	var lastAsk atomic.Value
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		lastAsk.Store(req["ask"])
		json.NewEncoder(w).Encode(map[string]any{"analysis": holmesAnswer, "tool_calls": []any{1}})
	}))
	defer hs.Close()

	store, _ := incident.NewFileStore(t.TempDir())
	t0 := time.Date(2026, 10, 1, 4, 24, 9, 0, time.UTC)
	primary := newIncidentAt(t, store, "rule-3", "worker01", t0)
	member := newIncidentAt(t, store, "rule-5", "worker01", t0)
	_, _ = store.Correlate(primary.IncidentID, 10*time.Minute, nil)
	_, _ = store.Correlate(member.IncidentID, 10*time.Minute, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := investigate.New(ctx, investigate.Options{
		Store: store, Holmes: holmes.New(hs.URL, "", time.Second), GroupWait: 50 * time.Millisecond, Workers: 2,
	})
	_ = r.Enqueue(primary.IncidentID, false)
	_ = r.Enqueue(member.IncidentID, false)

	waitStatus(t, store, primary.IncidentID, "done")
	m := waitStatus(t, store, member.IncidentID, "grouped")
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("holmes called %d times for one group, want 1", calls.Load())
	}
	if !strings.Contains(m.HolmesError, primary.IncidentID) {
		t.Errorf("member should point at its primary: %q", m.HolmesError)
	}
	ask, _ := lastAsk.Load().(string)
	if !strings.Contains(ask, member.IncidentID) || !strings.Contains(ask, "rule_id=rule-5") {
		t.Errorf("primary ask must list the related alert")
	}

	md := report.Markdown(m)
	if !strings.Contains(md, "grouped into "+primary.IncidentID) {
		t.Errorf("member report should point at the primary")
	}
}

type fakeChecker struct{ calls atomic.Int32 }

func (f *fakeChecker) Check(_ context.Context, in check.Input) (check.Verdict, error) {
	n := f.calls.Add(1)
	if n == 1 {
		return check.Verdict{Provider: "fake", Consistent: 0.1, SupportedStatus: "confirmed"}, nil
	}
	return check.Verdict{Provider: "fake", Consistent: 0.9, SupportedStatus: "confirmed"}, nil
}

func TestEvalRunDerivedJevRechecksOnceAndLeavesIncidentAlone(t *testing.T) {
	var asks atomic.Int32
	var lastAsk atomic.Value
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		lastAsk.Store(req["ask"])
		json.NewEncoder(w).Encode(map[string]any{"analysis": holmesAnswer, "tool_calls": []any{1, 2}})
	}))
	defer hs.Close()
	store, _ := incident.NewFileStore(t.TempDir())
	inc := newIncident(t, store, "rule-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := &fakeChecker{}
	r := investigate.New(ctx, investigate.Options{
		Store: store, Holmes: holmes.New(hs.URL, "", time.Second), Checker: fc, EvalDir: t.TempDir(),
	})

	id, err := r.StartEval(inc.IncidentID, "derived+jev")
	if err != nil {
		t.Fatal(err)
	}
	var run investigate.EvalRun
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := r.EvalResult(id)
		if err == nil && json.Unmarshal(b, &run) == nil && (run.Status == "done" || run.Status == "failed") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	o := run.Outcome
	if run.Status != "done" || o == nil || !o.Rechecked || o.HolmesCalls != 2 || len(o.Checks) != 2 || o.ToolCalls != 4 {
		t.Fatalf("run=%+v outcome=%+v", run, o)
	}
	if s, _ := lastAsk.Load().(string); !strings.Contains(s, "independent checker found your previous conclusion inconsistent") {
		t.Fatalf("second ask lacks feedback: %.200s", s)
	}
	got, _ := store.Get(inc.IncidentID)
	if got.HolmesStatus != "" || got.HolmesAttempts != 0 {
		t.Fatalf("eval changed the incident: status=%q attempts=%d", got.HolmesStatus, got.HolmesAttempts)
	}
	if _, err := r.StartEval(inc.IncidentID, "bogus"); err == nil {
		t.Fatal("want error for unknown arch")
	}
}

func TestOutcomeRecordsDuration(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"analysis": holmesAnswer})
	}))
	defer hs.Close()
	store, _ := incident.NewFileStore(t.TempDir())
	inc := newIncident(t, store, "rule-1")
	r := investigate.New(context.Background(), investigate.Options{Store: store, Holmes: holmes.New(hs.URL, "", time.Second)})
	if o := r.Investigate(context.Background(), inc, nil, investigate.ArchBaseline); o.DurationMS < 20 {
		t.Fatalf("duration_ms=%d", o.DurationMS)
	}
}

func TestEvalRunUsesEvalModelWhenSet(t *testing.T) {
	models := make(chan string, 4)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		models <- req["model"]
		json.NewEncoder(w).Encode(map[string]any{"analysis": holmesAnswer})
	}))
	defer hs.Close()
	store, _ := incident.NewFileStore(t.TempDir())
	inc := newIncident(t, store, "rule-1")
	r := investigate.New(context.Background(), investigate.Options{
		Store: store, EvalDir: t.TempDir(),
		Holmes: holmes.New(hs.URL, "gateway-luna", time.Second), Model: "gateway-luna",
		EvalHolmes: holmes.New(hs.URL, "gateway-luna-eval", time.Second), EvalModel: "gateway-luna-eval",
	})

	if o := r.Investigate(context.Background(), inc, nil, investigate.ArchBaseline); o.Model != "gateway-luna" {
		t.Fatalf("automatic outcome model=%q", o.Model)
	}
	id, err := r.StartEval(inc.IncidentID, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	var run investigate.EvalRun
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := r.EvalResult(id)
		if err == nil && json.Unmarshal(b, &run) == nil && (run.Status == "done" || run.Status == "failed") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != "done" || run.Outcome.Model != "gateway-luna-eval" {
		t.Fatalf("run=%+v", run)
	}
	if a, b := <-models, <-models; a != "gateway-luna" || b != "gateway-luna-eval" {
		t.Fatalf("holmes saw models %q then %q", a, b)
	}
}
