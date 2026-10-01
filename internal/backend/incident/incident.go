// Package incident defines the NetworkDoctor incident data model and the
// translation from an Alertmanager alert into an incident.
//
// The backend does not decide whether something is wrong: Prometheus rules
// own firing/resolved. The backend records the alert as an incident, enriches
// it, and drives the follow-up work (evidence, HolmesGPT, report).
package incident

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/alertmanager"
)

// Recovery status values.
const (
	RecoveryOpen     = "open"
	RecoveryResolved = "resolved"
)

// Incident is the persisted incident record. Field names follow the
// handoff's incident JSON model.
type Incident struct {
	IncidentID       string `json:"incident_id"`
	SourceAlertKey   string `json:"source_alert_key"`
	AlertFingerprint string `json:"alert_fingerprint"`
	AlertStatus      string `json:"alert_status"`

	Cluster string `json:"cluster"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	StartsAt  time.Time  `json:"starts_at"`
	EndsAt    *time.Time `json:"ends_at"`

	Severity string `json:"severity"`
	RuleID   string `json:"rule_id"`
	Scenario string `json:"scenario"`

	SymptomSummary   string   `json:"symptom_summary"`
	AffectedNodes    []string `json:"affected_nodes"`
	AffectedServices []string `json:"affected_services"`

	AlertLabels      map[string]string `json:"alert_labels"`
	AlertAnnotations map[string]string `json:"alert_annotations"`
	GeneratorURL     string            `json:"generator_url,omitempty"`
	ExternalURL      string            `json:"external_url,omitempty"`
	Receiver         string            `json:"receiver,omitempty"`
	GroupKey         string            `json:"group_key,omitempty"`

	RootCauseCandidates []RootCauseCandidate `json:"root_cause_candidates"`
	EvidenceMetrics     []Evidence           `json:"evidence_metrics"`
	RecommendedActions  []string             `json:"recommended_actions"`

	// HolmesResult is the parsed skill output schema (rule_id, root_cause,
	// trigger_evidence, ...). HolmesAttempts enforces one automatic call per
	// incident; only POST /incidents/{id}/holmes can trigger another.
	HolmesResult   map[string]any `json:"holmes_result"`
	HolmesAttempts int            `json:"holmes_attempts"`
	HolmesLastAt   *time.Time     `json:"holmes_last_at,omitempty"`
	// HolmesStatus: "" (never), queued, running, done, failed, skipped.
	HolmesStatus   string `json:"holmes_status,omitempty"`
	HolmesModel    string `json:"holmes_model,omitempty"`
	HolmesError    string `json:"holmes_error,omitempty"`
	HolmesAnalysis string `json:"holmes_analysis,omitempty"`
	// HolmesToolCalls is how many tool calls Holmes made; a cheap signal of
	// how hard it had to look.
	HolmesToolCalls int `json:"holmes_tool_calls,omitempty"`

	RecoveryStatus string `json:"recovery_status"`

	// DeliveryCount counts webhook deliveries that touched this incident.
	// It is diagnostic only: it shows how often Alertmanager redelivered.
	DeliveryCount int `json:"delivery_count"`
}

// RootCauseCandidate is one scored root-cause hypothesis.
type RootCauseCandidate struct {
	Cause      string  `json:"cause"`
	Confidence string  `json:"confidence"`
	Score      float64 `json:"score,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// Evidence is one PromQL observation attached to the incident.
type Evidence struct {
	Metric      string `json:"metric"`
	Query       string `json:"query,omitempty"`
	Observation string `json:"observation"`
	Source      string `json:"source,omitempty"`
}

// Meta carries payload-level fields that are copied into each incident.
type Meta struct {
	Cluster     string
	ExternalURL string
	Receiver    string
	GroupKey    string
}

// IDForKey derives a deterministic incident ID from the source key so a
// redelivered alert maps to the same ID even if the store index is rebuilt.
func IDForKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "inc-" + hex.EncodeToString(sum[:6])
}

// FromAlert builds a new incident from a firing (or resolved) alert.
func FromAlert(a alertmanager.Alert, meta Meta, now time.Time) *Incident {
	key := a.SourceKey()
	fp := a.Fingerprint
	if fp == "" {
		fp = alertmanager.FallbackFingerprint(a.Labels)
	}

	inc := &Incident{
		IncidentID:       IDForKey(key),
		SourceAlertKey:   key,
		AlertFingerprint: fp,
		Cluster:          meta.Cluster,
		CreatedAt:        now,
		StartsAt:         a.StartsAt.UTC(),
		AlertLabels:      copyMap(a.Labels),
		AlertAnnotations: copyMap(a.Annotations),
		GeneratorURL:     a.GeneratorURL,
		ExternalURL:      meta.ExternalURL,
		Receiver:         meta.Receiver,
		GroupKey:         meta.GroupKey,

		RootCauseCandidates: []RootCauseCandidate{},
		EvidenceMetrics:     []Evidence{},
		RecommendedActions:  []string{},
	}
	inc.applyClassification(a)
	inc.ApplyDelivery(a, now)
	return inc
}

// ApplyDelivery updates an existing incident from a redelivered alert.
// Status transitions are monotonic towards resolved: a late "firing"
// redelivery after a "resolved" one does not reopen the incident.
func (inc *Incident) ApplyDelivery(a alertmanager.Alert, now time.Time) {
	inc.DeliveryCount++
	inc.UpdatedAt = now

	// Labels/annotations are keyed by fingerprint so they cannot change, but
	// annotations are not part of the fingerprint and may be edited in the
	// rule; keep the latest copy.
	if len(a.Annotations) > 0 {
		inc.AlertAnnotations = copyMap(a.Annotations)
	}
	if a.GeneratorURL != "" {
		inc.GeneratorURL = a.GeneratorURL
	}

	if a.IsResolved() {
		inc.AlertStatus = alertmanager.StatusResolved
		inc.RecoveryStatus = RecoveryResolved
		if a.EndsAtValid() {
			t := a.EndsAt.UTC()
			inc.EndsAt = &t
		} else if inc.EndsAt == nil {
			t := now
			inc.EndsAt = &t
		}
		return
	}

	if inc.AlertStatus == alertmanager.StatusResolved {
		// Already resolved; ignore stale firing redelivery.
		return
	}
	inc.AlertStatus = alertmanager.StatusFiring
	inc.RecoveryStatus = RecoveryOpen
}

// applyClassification fills the descriptive fields from labels/annotations.
// It only reads what the Prometheus rule already decided; no detection here.
func (inc *Incident) applyClassification(a alertmanager.Alert) {
	l := a.Labels
	inc.Severity = firstNonEmpty(l["severity"], "unknown")
	inc.RuleID = firstNonEmpty(l["rule_id"], l["alertname"])
	inc.Scenario = firstNonEmpty(l["scenario"], l["alertname"])
	inc.SymptomSummary = firstNonEmpty(a.Annotations["summary"], a.Annotations["description"], l["alertname"])

	inc.AffectedNodes = uniqueNonEmpty(l["node"], l["nodename"], l["kubernetes_io_hostname"])
	if len(inc.AffectedNodes) == 0 {
		// No node-ish label (e.g. ServiceMonitor relabeling not applied):
		// fall back to the scrape target host.
		inc.AffectedNodes = uniqueNonEmpty(hostOf(l["instance"]))
	}
	// Scenario rules rewrite `namespace` to the NetworkDoctor namespace for
	// routing and keep the namespace they are about in `target_namespace`.
	ns := firstNonEmpty(l["target_namespace"], l["namespace"])
	inc.AffectedServices = uniqueNonEmpty(qualified(ns, l["service"]), qualified(ns, l["workload"]))
}

func hostOf(instance string) string {
	if instance == "" {
		return ""
	}
	if i := strings.LastIndex(instance, ":"); i > 0 {
		return instance[:i]
	}
	return instance
}

func qualified(ns, name string) string {
	if name == "" {
		return ""
	}
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func uniqueNonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	seen := map[string]bool{}
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
