// Package report renders an incident as a Markdown report.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/incident"
)

// Markdown renders one incident with its own diagnosis.
func Markdown(inc *incident.Incident) string {
	return MarkdownWithResult(inc, incident.ResolveResult(inc, nil))
}

// MarkdownWithResult renders the incident in the same order as the dashboard
// detail page (headline, analysis, evidence, excluded causes, actions, then
// details) using the selected diagnosis, while keeping the requested
// incident's identity, symptom, alert status and affected resources.
func MarkdownWithResult(inc *incident.Incident, view incident.ResultView) string {
	var b strings.Builder
	source := view.Source
	if source == nil {
		source = inc
	}
	r := view.Result
	h := inc.HeadlineFor(r)
	fmt.Fprintf(&b, "# %s\n\n", inc.ScenarioName())
	fmt.Fprintf(&b, "_%s · %s · %s 생성_\n\n", inc.IncidentID, inc.Cluster, time.Now().UTC().Format(time.RFC3339))

	fmt.Fprintf(&b, "**증상** · %s\n\n", h.Symptom)
	if h.Cause != "" {
		label := h.CauseLabel
		if view.Previous {
			label = "이전 분석 · " + label
		}
		fmt.Fprintf(&b, "**%s** · %s\n\n", label, h.Cause)
	}
	meta := []string{"발생 " + inc.StartsAt.Format(time.RFC3339)}
	if c := incident.ConfidenceName(str(r["confidence"])); c != "" {
		meta = append([]string{"신뢰도 " + c}, meta...)
	}
	fmt.Fprintf(&b, "_%s_\n\n", strings.Join(meta, " · "))
	if view.Shared {
		fmt.Fprintf(&b, "함께 묶인 `%s` 인시던트의 분석 결과입니다.\n\n", view.SourceIncidentID)
	}
	if view.Previous {
		fmt.Fprintf(&b, "이전 시도의 결과입니다. 현재 분석 상태: %s.\n\n", view.SourceHolmesStatus)
	}

	if r != nil {
		// Without summary.cause the headline already is root_cause.
		if rc := strings.TrimSpace(str(r["root_cause"])); rc != "" && rc != h.Cause {
			fmt.Fprintf(&b, "## 원인 분석\n\n%s\n\n", rc)
		}
		if hasItems(r["trigger_evidence"]) || hasItems(r["supporting_evidence"]) {
			b.WriteString("## 근거\n\n")
			section(&b, "알림을 일으킨 신호", r["trigger_evidence"])
			section(&b, "함께 움직인 신호", r["supporting_evidence"])
		}
		if hasItems(r["excluded_alternatives"]) {
			b.WriteString("## 배제한 원인\n\n")
			section(&b, "", r["excluded_alternatives"])
		}
		if hasItems(r["recommended_actions"]) || hasItems(r["additional_checks"]) {
			b.WriteString("## 권장 조치\n\n")
			section(&b, "", r["recommended_actions"])
			section(&b, "추가로 확인하면 좋은 것", r["additional_checks"])
		}
	} else if view.Unavailable {
		b.WriteString("## 원인 분석\n\n함께 묶인 대표 인시던트의 분석을 찾을 수 없습니다.\n\n")
	} else if source.HolmesError != "" {
		fmt.Fprintf(&b, "## 원인 분석\n\n분석 실패: %s\n\n", source.HolmesError)
	}

	b.WriteString("## 상세 정보\n\n")
	b.WriteString("| 항목 | 값 |\n| --- | --- |\n")
	row(&b, "상태", statusName(inc.AlertStatus))
	row(&b, "룰 / 시나리오", inc.RuleID+" / "+inc.Scenario)
	row(&b, "발생", inc.StartsAt.Format(time.RFC3339))
	if inc.EndsAt != nil {
		row(&b, "복구", inc.EndsAt.Format(time.RFC3339))
	}
	row(&b, "노드", strings.Join(inc.AffectedNodes, ", "))
	row(&b, "서비스", strings.Join(inc.AffectedServices, ", "))
	row(&b, "알림 요약", inc.SymptomSummary)
	if view.Unavailable {
		row(&b, "원인 분석", "그룹 분석 없음")
	} else {
		row(&b, "원인 분석", holmesLine(source))
	}
	if inc.IsGroupMember() {
		row(&b, "상관 그룹", inc.CorrelationID+"에 묶임")
	} else if len(inc.CorrelatedIncidents) > 0 {
		row(&b, "상관 그룹", "대표 인시던트. 함께 묶인 알림: "+strings.Join(inc.CorrelatedIncidents, ", "))
	}
	b.WriteString("\n")

	if !view.Unavailable && len(source.EvidenceMetrics) > 0 {
		b.WriteString("### NetworkDoctor 수집 지표\n\n")
		for _, e := range source.EvidenceMetrics {
			fmt.Fprintf(&b, "- **%s**: %s\n  `%s`\n", e.Metric, e.Observation, e.Query)
		}
		b.WriteString("\n")
	}

	b.WriteString("### 알림 레이블\n\n")
	keys := make([]string, 0, len(inc.AlertLabels))
	for k := range inc.AlertLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- `%s` = `%s`\n", k, inc.AlertLabels[k])
	}
	if inc.GeneratorURL != "" {
		fmt.Fprintf(&b, "\n원본 쿼리: %s\n", inc.GeneratorURL)
	}
	return b.String()
}

func statusName(s string) string {
	return firstNonEmpty(map[string]string{"firing": "진행 중", "resolved": "복구됨"}[s], s)
}

func hasItems(v any) bool {
	items, ok := v.([]any)
	return ok && len(items) > 0
}

func holmesLine(inc *incident.Incident) string {
	if inc.HolmesStatus == "" {
		return "시작 안 함"
	}
	s := inc.HolmesStatus
	if inc.HolmesModel != "" {
		s += " · 모델 " + inc.HolmesModel
	}
	if inc.HolmesToolCalls > 0 {
		s += fmt.Sprintf(" · 도구 호출 %d회", inc.HolmesToolCalls)
	}
	if inc.HolmesTotalTokens > 0 {
		s += fmt.Sprintf(" · 토큰 %d (입력 %d / 출력 %d)",
			inc.HolmesTotalTokens, inc.HolmesPromptTokens, inc.HolmesCompletionTokens)
	}
	if inc.HolmesCostUSD > 0 {
		s += fmt.Sprintf(" · $%.4f", inc.HolmesCostUSD)
	}
	return s
}

func section(b *strings.Builder, title string, v any) {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return
	}
	if title != "" {
		fmt.Fprintf(b, "### %s\n\n", title)
	}
	for _, it := range items {
		switch t := it.(type) {
		case string:
			fmt.Fprintf(b, "- %s\n", strings.TrimSpace(t))
		case map[string]any:
			body := strings.TrimSpace(firstNonEmpty(str(t["observation"]), str(t["reason"])))
			if hyp := strings.TrimSpace(str(t["hypothesis"])); hyp != "" {
				// Excluded alternative: name it first, same as the dashboard.
				fmt.Fprintf(b, "- **%s**: %s\n", hyp, body)
			} else if src := firstNonEmpty(str(t["source"]), str(t["metric_or_tool"])); src != "" {
				// Evidence: the observation first, its query underneath.
				fmt.Fprintf(b, "- %s  \n  `%s`\n", body, strings.TrimSpace(src))
			} else {
				fmt.Fprintf(b, "- %s\n", body)
			}
		default:
			fmt.Fprintf(b, "- %v\n", t)
		}
	}
	b.WriteString("\n")
}

func row(b *strings.Builder, k, v string) {
	if v == "" || v == " / " {
		v = "-"
	}
	fmt.Fprintf(b, "| %s | %s |\n", k, strings.ReplaceAll(v, "|", "\\|"))
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
