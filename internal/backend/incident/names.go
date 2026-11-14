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

// Headline is the summary at the top of the report and on each list row:
// what users saw, the verdict on the alert's scenario, and the cause line.
type Headline struct {
	Symptom    string `json:"symptom"`
	Verdict    string `json:"verdict,omitempty"`
	CauseLabel string `json:"cause_label,omitempty"`
	Cause      string `json:"cause,omitempty"`
}

// causeLabels name the cause line by investigation_status. An excluded result
// still names the likelier cause, so only inconclusive gets its own label.
var causeLabels = map[string]string{
	"inconclusive": "보류 이유",
}

// verdict says in plain words whether the alert's scenario held, e.g.
// "네트워크 혼잡 아님", so readers do not have to decode the status enum.
func (inc *Incident) verdict(status string) string {
	name := firstNonEmpty(scenarioNames[inc.Scenario], "이 시나리오")
	switch status {
	case "confirmed":
		return name + " 맞음"
	case "excluded":
		return name + " 아님"
	case "inconclusive":
		return "판단 보류"
	}
	return ""
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
	st, _ := r["investigation_status"].(string)
	h.Verdict = inc.verdict(st)
	if h.Cause != "" {
		h.CauseLabel = firstNonEmpty(causeLabels[st], "추정 원인")
	}
	return h
}

// ConfidenceName is the Korean label of a confidence value.
func ConfidenceName(c string) string {
	return map[string]string{"high": "높음", "medium": "보통", "low": "낮음"}[c]
}
