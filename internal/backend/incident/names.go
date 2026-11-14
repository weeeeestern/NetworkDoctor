package incident

import "strings"

// scenarioNames are the Korean display names of the scenario skills. The
// dashboard and the Markdown report both read this one table.
var scenarioNames = map[string]string{
	"network-congestion":                     "네트워크 혼잡",
	"kernel-network-bottleneck":              "커널 네트워크 병목",
	"conntrack-exhaustion":                   "conntrack 고갈",
	"coredns-degradation":                    "CoreDNS 성능 저하",
	"dns-conntrack-correlation":              "DNS·conntrack 연쇄 장애",
	"node-localized-failure":                 "특정 노드 장애",
	"application-induced-network-bottleneck": "애플리케이션발 네트워크 병목",
	"networkpolicy-misconfiguration":         "NetworkPolicy 설정 오류",
}

// ScenarioName is the Korean display name of the incident's scenario, or the
// raw scenario / rule_id / incident_id when there is none (smoke rules).
func (inc *Incident) ScenarioName() string {
	if n, ok := scenarioNames[inc.Scenario]; ok {
		return n
	}
	return firstNonEmpty(inc.Scenario, inc.RuleID, inc.IncidentID)
}

// Headline is the two-line summary at the top of the report and on each
// list row: what users saw, and the cause line labelled by the conclusion.
type Headline struct {
	Symptom    string `json:"symptom"`
	CauseLabel string `json:"cause_label,omitempty"`
	Cause      string `json:"cause,omitempty"`
}

// causeLabels name the cause line by investigation_status.
var causeLabels = map[string]string{
	"confirmed":    "추정 원인",
	"excluded":     "이 시나리오 아님",
	"inconclusive": "원인 불확실",
}

// Headline is HeadlineFor the incident's own result.
func (inc *Incident) Headline() Headline { return inc.HeadlineFor(inc.HolmesResult) }

// HeadlineFor builds the headline from a diagnosis r (the incident's own, or
// its group's shared one from ResolveResult). It prefers the skill's summary
// block and falls back to the alert summary and root_cause for results
// written before summary existed.
func (inc *Incident) HeadlineFor(r map[string]any) Headline {
	h := Headline{Symptom: firstNonEmpty(inc.SymptomSummary, inc.ScenarioName())}
	if r == nil {
		return h
	}
	sum, _ := r["summary"].(map[string]any)
	if s, _ := sum["symptom"].(string); s != "" {
		h.Symptom = s
	}
	c, _ := sum["cause"].(string)
	if c == "" {
		c, _ = r["root_cause"].(string)
	}
	h.Cause = strings.TrimSpace(c)
	if h.Cause != "" {
		st, _ := r["investigation_status"].(string)
		h.CauseLabel = firstNonEmpty(causeLabels[st], "추정 원인")
	}
	return h
}

// ConfidenceName is the Korean label of a confidence value.
func ConfidenceName(c string) string {
	return map[string]string{"high": "높음", "medium": "보통", "low": "낮음"}[c]
}
