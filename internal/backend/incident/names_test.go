package incident

import "testing"

func TestScenarioName(t *testing.T) {
	if got := (&Incident{Scenario: "network-congestion"}).ScenarioName(); got != "네트워크 혼잡" {
		t.Errorf("known scenario = %q", got)
	}
	if got := (&Incident{Scenario: "smoke-tcp-retransmit-burst", RuleID: "smoke-1"}).ScenarioName(); got != "smoke-tcp-retransmit-burst" {
		t.Errorf("unknown scenario = %q, want the raw name", got)
	}
}

func TestHeadline(t *testing.T) {
	cases := []struct {
		name string
		inc  Incident
		want Headline
	}{
		{"no result", Incident{SymptomSummary: "p99 high"}, Headline{Symptom: "p99 high"}},
		{"root_cause fallback", Incident{
			SymptomSummary: "p99 high",
			HolmesResult:   map[string]any{"investigation_status": "confirmed", "root_cause": " link congestion "},
		}, Headline{Symptom: "p99 high", CauseLabel: "추정 원인", Cause: "link congestion"}},
		{"summary wins", Incident{
			SymptomSummary: "p99 high",
			HolmesResult: map[string]any{
				"investigation_status": "excluded",
				"root_cause":           "long paragraph",
				"summary":              map[string]any{"symptom": "catshop 응답 지연", "cause": "앱이 직접 500 반환"},
			},
		}, Headline{Symptom: "catshop 응답 지연", CauseLabel: "이 시나리오 아님", Cause: "앱이 직접 500 반환"}},
		{"unknown status", Incident{
			Scenario:     "coredns-degradation",
			HolmesResult: map[string]any{"root_cause": "x"},
		}, Headline{Symptom: "CoreDNS 성능 저하", CauseLabel: "추정 원인", Cause: "x"}},
	}
	for _, c := range cases {
		if got := c.inc.Headline(); got != c.want {
			t.Errorf("%s: Headline = %+v, want %+v", c.name, got, c.want)
		}
	}
}
