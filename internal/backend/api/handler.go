// Package api exposes the NetworkDoctor backend HTTP API.
//
// Milestone 1 surface:
//
//	GET  /healthz
//	POST /webhooks/alertmanager
//	GET  /incidents
//	GET  /incidents/{id}
//	POST /incidents/{id}/holmes   (reserved; returns 501 until the Holmes milestone)
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
)

// Options configures the handler.
type Options struct {
	Store        *incident.FileStore
	ClusterName  string
	MaxBodyBytes int64
	Now          func() time.Time
	Logger       *log.Logger
}

type handler struct {
	store        *incident.FileStore
	cluster      string
	maxBodyBytes int64
	now          func() time.Time
	log          *log.Logger
}

// New returns the HTTP handler for the backend.
func New(opts Options) http.Handler {
	h := &handler{
		store:        opts.Store,
		cluster:      opts.ClusterName,
		maxBodyBytes: opts.MaxBodyBytes,
		now:          opts.Now,
		log:          opts.Logger,
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
	mux.HandleFunc("POST /incidents/{id}/holmes", h.holmesNotImplemented)
	return mux
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

func (h *handler) holmesNotImplemented(w http.ResponseWriter, r *http.Request) {
	if _, err := h.store.Get(r.PathValue("id")); err != nil {
		if errors.Is(err, incident.ErrNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read incident")
		return
	}
	writeError(w, http.StatusNotImplemented, "HolmesGPT integration is not implemented in this milestone")
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
