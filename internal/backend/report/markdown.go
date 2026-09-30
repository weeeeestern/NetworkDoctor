// Package report renders an incident as a Markdown report.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/incident"
)

// Markdown renders one incident.
func Markdown(inc *incident.Incident) string {
	var b strings.Builder
	title := firstNonEmpty(inc.AlertLabels["alertname"], inc.Scenario, inc.IncidentID)
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "_%s · %s · generated %s_\n\n", inc.IncidentID, inc.Cluster, time.Now().UTC().Format(time.RFC3339))

	b.WriteString("| Field | Value |\n| --- | --- |\n")
	row(&b, "Status", fmt.Sprintf("%s (recovery: %s)", inc.AlertStatus, inc.RecoveryStatus))
	row(&b, "Severity", inc.Severity)
	row(&b, "Rule / scenario", inc.RuleID+" / "+inc.Scenario)
	row(&b, "Started", inc.StartsAt.Format(time.RFC3339))
	if inc.EndsAt != nil {
		row(&b, "Ended", inc.EndsAt.Format(time.RFC3339))
	}
	row(&b, "Nodes", strings.Join(inc.AffectedNodes, ", "))
	row(&b, "Services", strings.Join(inc.AffectedServices, ", "))
	row(&b, "Summary", inc.SymptomSummary)
	row(&b, "Investigation", holmesLine(inc))
	b.WriteString("\n")

	r := inc.HolmesResult
	if r != nil {
		b.WriteString("## Root cause\n\n")
		fmt.Fprintf(&b, "**%s** · confidence **%s**\n\n", str(r["investigation_status"]), str(r["confidence"]))
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(str(r["root_cause"])))
		section(&b, "Trigger evidence", r["trigger_evidence"])
		section(&b, "Supporting evidence", r["supporting_evidence"])
		section(&b, "Excluded alternatives", r["excluded_alternatives"])
		section(&b, "Recommended actions", r["recommended_actions"])
		section(&b, "Additional checks", r["additional_checks"])
	} else if inc.HolmesError != "" {
		fmt.Fprintf(&b, "## Root cause\n\nNot available: %s\n\n", inc.HolmesError)
	}

	if len(inc.EvidenceMetrics) > 0 {
		b.WriteString("## Evidence collected by NetworkDoctor\n\n")
		for _, e := range inc.EvidenceMetrics {
			fmt.Fprintf(&b, "- **%s**: %s\n  `%s`\n", e.Metric, e.Observation, e.Query)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Alert labels\n\n")
	keys := make([]string, 0, len(inc.AlertLabels))
	for k := range inc.AlertLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- `%s` = `%s`\n", k, inc.AlertLabels[k])
	}
	if inc.GeneratorURL != "" {
		fmt.Fprintf(&b, "\nSource query: %s\n", inc.GeneratorURL)
	}
	return b.String()
}

func holmesLine(inc *incident.Incident) string {
	if inc.HolmesStatus == "" {
		return "not started"
	}
	s := inc.HolmesStatus
	if inc.HolmesModel != "" {
		s += " · model " + inc.HolmesModel
	}
	if inc.HolmesToolCalls > 0 {
		s += fmt.Sprintf(" · %d tool calls", inc.HolmesToolCalls)
	}
	return s
}

func section(b *strings.Builder, title string, v any) {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "### %s\n\n", title)
	for _, it := range items {
		switch t := it.(type) {
		case string:
			fmt.Fprintf(b, "- %s\n", strings.TrimSpace(t))
		case map[string]any:
			head := firstNonEmpty(str(t["source"]), str(t["hypothesis"]), str(t["metric_or_tool"]))
			body := firstNonEmpty(str(t["observation"]), str(t["reason"]))
			fmt.Fprintf(b, "- **%s**: %s\n", strings.TrimSpace(head), strings.TrimSpace(body))
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
