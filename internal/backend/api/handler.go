// Package api exposes the NetworkDoctor backend HTTP API.
//
// Milestone 1 surface:
//
//	GET  /healthz
//	POST /webhooks/alertmanager
//	GET  /incidents
//	GET  /incidents/{id}
//	GET  /incidents/{id}/report.md
//	POST /incidents/{id}/holmes   (manual re-investigation)
//	POST /eval/runs               (re-investigate with an architecture, no state change)
//	GET  /eval/runs/{run}
//	GET  /                        (embedded incident dashboard)
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"networkdoctor-agent/internal/backend/alertmanager"
	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/report"
	"networkdoctor-agent/internal/backend/webui"
)

// Investigator schedules background investigations. nil disables them.
type Investigator interface {
	Enqueue(id string, force bool) error
}

// Evaluator runs evaluation investigations (optional; implemented by the
// investigate.Runner).
type Evaluator interface {
	StartEval(incidentID, arch string) (string, error)
	EvalResult(runID string) ([]byte, error)
}

// Options configures the handler.
type Options struct {
	Store        *incident.FileStore
	ClusterName  string
	MaxBodyBytes int64
	Now          func() time.Time
	Logger       *log.Logger
	Investigator Investigator
	// AutoInvestigate enqueues new firing incidents automatically.
	AutoInvestigate bool
	// CorrelationWindow groups new incidents with an existing one on the same
	// node/service started within this window (0 disables grouping).
	CorrelationWindow time.Duration
	// Eligible decides which incidents take part in grouping (nil = all).
	Eligible func(*incident.Incident) bool
}

type handler struct {
	store        *incident.FileStore
	cluster      string
	maxBodyBytes int64
	now          func() time.Time
	log          *log.Logger
	inv          Investigator
	auto         bool
	window       time.Duration
	eligible     func(*incident.Incident) bool
}

// New returns the HTTP handler for the backend.
func New(opts Options) http.Handler {
	h := &handler{
		store:        opts.Store,
		cluster:      opts.ClusterName,
		maxBodyBytes: opts.MaxBodyBytes,
		now:          opts.Now,
		log:          opts.Logger,
		inv:          opts.Investigator,
		auto:         opts.AutoInvestigate,
		window:       opts.CorrelationWindow,
		eligible:     opts.Eligible,
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.log == nil {
		h.log = log.Default()
	}
	if h.maxBodyBytes <= 0 {
		h.maxBodyBytes = 4 << 20
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("POST /webhooks/alertmanager", h.alertmanagerWebhook)
	mux.HandleFunc("GET /incidents", h.listIncidents)
	mux.HandleFunc("GET /incidents/{id}", h.getIncident)
	mux.HandleFunc("GET /incidents/{id}/report.md", h.reportMarkdown)
	mux.HandleFunc("POST /incidents/{id}/holmes", h.holmesRetry)
	mux.HandleFunc("POST /eval/runs", h.evalStart)
	mux.HandleFunc("GET /eval/runs/{run}", h.evalGet)
	// Dashboard: "GET /" only matches paths no API route claims, so the UI
	// and its assets live at the root without shadowing the endpoints above.
	mux.Handle("GET /", webui.Handler())
	return mux
}

type evalRequest struct {
	IncidentID string `json:"incident_id"`
	Arch       string `json:"arch"`
}

func (h *handler) evaluator() (Evaluator, bool) {
	e, ok := h.inv.(Evaluator)
	return e, ok && h.inv != nil
}

// evalStart re-investigates an existing incident with the requested
// architecture. It returns 202 with a run id; poll GET /eval/runs/{run}.
func (h *handler) evalStart(w http.ResponseWriter, r *http.Request) {
	ev, ok := h.evaluator()
	if !ok {
		writeError(w, http.StatusNotImplemented, "investigations are not configured")
		return
	}
	var req evalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.IncidentID == "" {
		writeError(w, http.StatusBadRequest, `body must be {"incident_id": "...", "arch": "baseline|derived|derived+jev"}`)
		return
	}
	id, err := ev.StartEval(req.IncidentID, req.Arch)
	switch {
	case errors.Is(err, incident.ErrNotFound):
		writeError(w, http.StatusNotFound, "incident not found")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": id, "incident_id": req.IncidentID, "arch": req.Arch})
}

func (h *handler) evalGet(w http.ResponseWriter, r *http.Request) {
	ev, ok := h.evaluator()
	if !ok {
		writeError(w, http.StatusNotImplemented, "investigations are not configured")
		return
	}
	b, err := ev.EvalResult(r.PathValue("run"))
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// WebhookResponse is the JSON body returned to Alertmanager. Alertmanager
// only cares about the 2xx status, but the body makes curl-driven testing
// self-explanatory.
type WebhookResponse struct {
	Received  int      `json:"received"`
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Incidents []string `json:"incidents"`
}

func (h *handler) alertmanagerWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	dec := json.NewDecoder(r.Body)

	var payload alertmanager.Payload
	if err := dec.Decode(&payload); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid webhook payload: %v", err))
		return
	}
	if len(payload.Alerts) == 0 {
		writeError(w, http.StatusBadRequest, "webhook payload contains no alerts")
		return
	}

	meta := incident.Meta{
		Cluster:     h.cluster,
		ExternalURL: payload.ExternalURL,
		Receiver:    payload.Receiver,
		GroupKey:    payload.GroupKey,
	}
	now := h.now().UTC()

	resp := WebhookResponse{Received: len(payload.Alerts), Incidents: make([]string, 0, len(payload.Alerts))}
	for _, a := range payload.Alerts {
		if a.StartsAt.IsZero() {
			writeError(w, http.StatusBadRequest, "alert is missing startsAt")
			return
		}
		key := a.SourceKey()
		inc, created, err := h.store.Upsert(key, func(existing *incident.Incident) (*incident.Incident, error) {
			if existing == nil {
				return incident.FromAlert(a, meta, now), nil
			}
			existing.ApplyDelivery(a, now)
			return existing, nil
		})
		if err != nil {
			h.log.Printf("webhook: upsert %s failed: %v", key, err)
			// 5xx makes Alertmanager retry, which is what we want for a
			// transient storage failure.
			writeError(w, http.StatusInternalServerError, "failed to persist incident")
			return
		}
		if created {
			resp.Created++
		} else {
			resp.Updated++
		}
		resp.Incidents = append(resp.Incidents, inc.IncidentID)
		if created && !a.IsResolved() {
			if c, err := h.store.Correlate(inc.IncidentID, h.window, h.eligible); err != nil {
				h.log.Printf("webhook: correlate %s: %v", inc.IncidentID, err)
			} else if c.IsGroupMember() {
				h.log.Printf("webhook: incident %s joins correlation group %s", inc.IncidentID, c.CorrelationID)
			}
		}
		if created && !a.IsResolved() && h.auto && h.inv != nil {
			if err := h.inv.Enqueue(inc.IncidentID, false); err != nil {
				h.log.Printf("webhook: enqueue investigation %s: %v", inc.IncidentID, err)
			}
		}
		h.log.Printf("webhook: %s incident=%s alert=%s status=%s created=%t deliveries=%d",
			payload.Receiver, inc.IncidentID, a.Labels["alertname"], a.Status, created, inc.DeliveryCount)
	}
	writeJSON(w, http.StatusOK, resp)
}

// IncidentSummary is the list view of an incident.
type IncidentSummary struct {
	IncidentID     string     `json:"incident_id"`
	AlertStatus    string     `json:"alert_status"`
	RecoveryStatus string     `json:"recovery_status"`
	Severity       string     `json:"severity"`
	RuleID         string     `json:"rule_id"`
	Scenario       string     `json:"scenario"`
	SymptomSummary string     `json:"symptom_summary"`
	StartsAt       time.Time  `json:"starts_at"`
	EndsAt         *time.Time `json:"ends_at"`
	AffectedNodes  []string   `json:"affected_nodes"`
	DeliveryCount  int        `json:"delivery_count"`
	HolmesStatus   string     `json:"holmes_status,omitempty"`
	CorrelationID  string     `json:"correlation_id,omitempty"`
}

func (h *handler) listIncidents(w http.ResponseWriter, r *http.Request) {
	all, err := h.store.List()
	if err != nil {
		h.log.Printf("list incidents: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list incidents")
		return
	}
	statusFilter := r.URL.Query().Get("status")
	out := make([]IncidentSummary, 0, len(all))
	for _, inc := range all {
		if statusFilter != "" && inc.AlertStatus != statusFilter {
			continue
		}
		out = append(out, IncidentSummary{
			IncidentID:     inc.IncidentID,
			AlertStatus:    inc.AlertStatus,
			RecoveryStatus: inc.RecoveryStatus,
			Severity:       inc.Severity,
			RuleID:         inc.RuleID,
			Scenario:       inc.Scenario,
			SymptomSummary: inc.SymptomSummary,
			StartsAt:       inc.StartsAt,
			EndsAt:         inc.EndsAt,
			AffectedNodes:  inc.AffectedNodes,
			DeliveryCount:  inc.DeliveryCount,
			HolmesStatus:   inc.HolmesStatus,
			CorrelationID:  inc.CorrelationID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": out, "count": len(out)})
}

func (h *handler) getIncident(w http.ResponseWriter, r *http.Request) {
	inc, err := h.store.Get(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, incident.ErrNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
			return
		}
		h.log.Printf("get incident: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to read incident")
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

func (h *handler) holmesRetry(w http.ResponseWriter, r *http.Request) {
	inc, err := h.store.Get(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, incident.ErrNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read incident")
		return
	}
	if h.inv == nil {
		writeError(w, http.StatusServiceUnavailable, "HolmesGPT is not configured (ND_HOLMES_URL is empty)")
		return
	}
	if inc.HolmesStatus == "running" {
		writeError(w, http.StatusConflict, "an investigation is already running for this incident")
		return
	}
	if err := h.inv.Enqueue(inc.IncidentID, true); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"incident_id": inc.IncidentID, "status": "queued"})
}

func (h *handler) reportMarkdown(w http.ResponseWriter, r *http.Request) {
	inc, err := h.store.Get(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, incident.ErrNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read incident")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(report.Markdown(inc)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
