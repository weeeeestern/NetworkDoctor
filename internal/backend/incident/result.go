package incident

import "strings"

// ResultView identifies the investigation used to present an incident's
// diagnosis while leaving that incident's own alert and recovery state intact.
type ResultView struct {
	SourceIncidentID   string         `json:"source_incident_id"`
	SourceHolmesStatus string         `json:"source_holmes_status"`
	SourceAttempt      int            `json:"source_attempt"`
	Shared             bool           `json:"shared"`
	Previous           bool           `json:"previous"`
	Unavailable        bool           `json:"unavailable"`
	Result             map[string]any `json:"result"`
	Source             *Incident      `json:"-"`
}

// ResolveResult selects an incident's own investigation or its group's primary
// in one lookup. A member that has attempted its own investigation stays
// independent even when that attempt failed or produced no result.
func ResolveResult(requested *Incident, lookup func(string) (*Incident, error)) ResultView {
	if requested == nil {
		return ResultView{Unavailable: true, SourceHolmesStatus: "unavailable"}
	}
	source := requested
	shared := requested.IsGroupMember() && requested.HolmesAttempts == 0 && requested.HolmesResult == nil
	if shared {
		unavailable := ResultView{SourceIncidentID: requested.CorrelationID, Shared: true, Unavailable: true, SourceHolmesStatus: "unavailable"}
		if lookup == nil {
			return unavailable
		}
		var err error
		source, err = lookup(requested.CorrelationID)
		if err != nil || source == nil || source.IncidentID != requested.CorrelationID || source.IsGroupMember() {
			return unavailable
		}
	}
	previous := source.HolmesResult != nil && (source.HolmesStatus == "queued" || source.HolmesStatus == "running" || source.HolmesStatus == "failed")
	return ResultView{
		SourceIncidentID: source.IncidentID, SourceHolmesStatus: source.HolmesStatus,
		SourceAttempt: source.HolmesAttempts, Shared: shared, Previous: previous,
		Result: source.HolmesResult, Source: source,
	}
}

// Completed reports whether the selected current attempt has a usable diagnosis.
func (r ResultView) Completed() bool {
	rootCause, ok := r.Result["root_cause"].(string)
	return r.SourceHolmesStatus == "done" && !r.Previous && !r.Unavailable && ok && strings.TrimSpace(rootCause) != ""
}
