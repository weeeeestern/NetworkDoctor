package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/links"
)

const firingPayload = `{
  "version": "4",
  "groupKey": "{}/{source=\"networkdoctor\"}:{alertname=\"NetworkDoctorTCPRetransmitBurstHigh\"}",
  "status": "firing",
  "receiver": "networkdoctor-backend",
  "groupLabels": {"alertname": "NetworkDoctorTCPRetransmitBurstHigh"},
  "commonLabels": {"alertname": "NetworkDoctorTCPRetransmitBurstHigh", "severity": "warning"},
  "commonAnnotations": {},
  "externalURL": "http://alertmanager.example",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "NetworkDoctorTCPRetransmitBurstHigh",
        "severity": "warning",
        "source": "networkdoctor",
        "rule_id": "smoke-1",
        "scenario": "smoke-tcp-retransmit-burst",
        "instance": "192.168.0.18:9102",
        "node": "worker01",
        "namespace": "networkdoctor",
        "service": "networkdoctor-agent"
      },
      "annotations": {"summary": "TCP retransmit bursts on worker01"},
      "startsAt": "2026-09-28T01:00:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus.example/graph?g0.expr=...",
      "fingerprint": "abcdef0123456789"
    }
  ]
}`

func newTestServer(t *testing.T) (*httptest.Server, *incident.FileStore) {
	t.Helper()
	store, err := incident.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 9, 28, 1, 5, 0, 0, time.UTC)
	h := New(Options{Store: store, ClusterName: "onprem-lab", Now: func() time.Time { return fixed }})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, store
}

func post(t *testing.T, url, body string) (int, WebhookResponse) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out WebhookResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

func TestWebhookIsIdempotentAcrossRedeliveries(t *testing.T) {
	srv, store := newTestServer(t)
	url := srv.URL + "/webhooks/alertmanager"

	code, first := post(t, url, firingPayload)
	if code != http.StatusOK || first.Created != 1 || first.Updated != 0 {
		t.Fatalf("first delivery: code=%d resp=%+v", code, first)
	}
	code, second := post(t, url, firingPayload)
	if code != http.StatusOK || second.Created != 0 || second.Updated != 1 {
		t.Fatalf("redelivery: code=%d resp=%+v", code, second)
	}
	if first.Incidents[0] != second.Incidents[0] {
		t.Fatalf("redelivery produced a different incident: %s vs %s", first.Incidents[0], second.Incidents[0])
	}

	all, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly one incident on disk, got %d", len(all))
	}
	inc := all[0]
	if inc.DeliveryCount != 2 {
		t.Errorf("delivery_count = %d, want 2", inc.DeliveryCount)
	}
	if inc.AlertStatus != "firing" || inc.RecoveryStatus != incident.RecoveryOpen {
		t.Errorf("status = %s/%s, want firing/open", inc.AlertStatus, inc.RecoveryStatus)
	}
	if inc.RuleID != "smoke-1" || inc.Scenario != "smoke-tcp-retransmit-burst" || inc.Severity != "warning" {
		t.Errorf("classification = %s/%s/%s", inc.RuleID, inc.Scenario, inc.Severity)
	}
	if len(inc.AffectedNodes) != 1 || inc.AffectedNodes[0] != "worker01" {
		t.Errorf("affected_nodes = %v, want [worker01] (instance host only as fallback)", inc.AffectedNodes)
	}
	if len(inc.AffectedServices) == 0 || inc.AffectedServices[0] != "networkdoctor/networkdoctor-agent" {
		t.Errorf("affected_services = %v", inc.AffectedServices)
	}
	if inc.Cluster != "onprem-lab" {
		t.Errorf("cluster = %q", inc.Cluster)
	}
	if inc.HolmesAttempts != 0 || inc.HolmesResult != nil {
		t.Errorf("holmes must not be invoked in this milestone: attempts=%d", inc.HolmesAttempts)
	}
}

func TestResolvedDeliveryClosesIncident(t *testing.T) {
	srv, store := newTestServer(t)
	url := srv.URL + "/webhooks/alertmanager"

	_, first := post(t, url, firingPayload)
	resolved := strings.ReplaceAll(firingPayload, `"status": "firing"`, `"status": "resolved"`)
	resolved = strings.ReplaceAll(resolved, `"endsAt": "0001-01-01T00:00:00Z"`, `"endsAt": "2026-09-28T01:10:00Z"`)
	code, second := post(t, url, resolved)
	if code != http.StatusOK || second.Updated != 1 {
		t.Fatalf("resolved delivery: code=%d resp=%+v", code, second)
	}
	if first.Incidents[0] != second.Incidents[0] {
		t.Fatalf("resolved delivery must map to the same incident")
	}

	inc, err := store.Get(first.Incidents[0])
	if err != nil {
		t.Fatal(err)
	}
	if inc.AlertStatus != "resolved" || inc.RecoveryStatus != incident.RecoveryResolved {
		t.Errorf("status = %s/%s, want resolved/resolved", inc.AlertStatus, inc.RecoveryStatus)
	}
	if inc.EndsAt == nil || !inc.EndsAt.Equal(time.Date(2026, 9, 28, 1, 10, 0, 0, time.UTC)) {
		t.Errorf("ends_at = %v", inc.EndsAt)
	}

	// A stale firing redelivery after resolution must not reopen it.
	post(t, url, firingPayload)
	inc, _ = store.Get(first.Incidents[0])
	if inc.AlertStatus != "resolved" {
		t.Errorf("stale firing reopened the incident: %s", inc.AlertStatus)
	}
}

func TestNewOccurrenceCreatesNewIncident(t *testing.T) {
	srv, _ := newTestServer(t)
	url := srv.URL + "/webhooks/alertmanager"

	_, first := post(t, url, firingPayload)
	later := strings.ReplaceAll(firingPayload, `"startsAt": "2026-09-28T01:00:00Z"`, `"startsAt": "2026-09-28T03:00:00Z"`)
	_, second := post(t, url, later)
	if second.Created != 1 {
		t.Fatalf("same fingerprint with a new startsAt must create a new incident: %+v", second)
	}
	if first.Incidents[0] == second.Incidents[0] {
		t.Fatalf("new occurrence reused the incident id")
	}
}

func TestMissingFingerprintUsesStableFallback(t *testing.T) {
	srv, _ := newTestServer(t)
	url := srv.URL + "/webhooks/alertmanager"

	noFP := strings.ReplaceAll(firingPayload, `"fingerprint": "abcdef0123456789"`, `"fingerprint": ""`)
	_, first := post(t, url, noFP)
	_, second := post(t, url, noFP)
	if first.Created != 1 || second.Updated != 1 || first.Incidents[0] != second.Incidents[0] {
		t.Fatalf("fallback key not stable: first=%+v second=%+v", first, second)
	}
}

func TestReadEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	_, created := post(t, srv.URL+"/webhooks/alertmanager", firingPayload)
	id := created.Incidents[0]

	resp, err := http.Get(srv.URL + "/incidents/" + id)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /incidents/{id} = %d", resp.StatusCode)
	}

	resp, _ = http.Get(srv.URL + "/incidents?status=firing")
	var list struct {
		Count int `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if list.Count != 1 {
		t.Errorf("GET /incidents?status=firing count = %d", list.Count)
	}

	resp, _ = http.Get(srv.URL + "/incidents/inc-000000000000")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", resp.StatusCode)
	}

	resp, _ = http.Get(srv.URL + "/incidents/../../etc/passwd")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("path traversal id must not succeed")
	}

	resp, _ = http.Post(srv.URL+"/incidents/"+id+"/holmes", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("POST /incidents/{id}/holmes without Holmes = %d, want 503", resp.StatusCode)
	}

	resp, _ = http.Get(srv.URL + "/incidents/" + id + "/report.md")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "# NetworkDoctorTCPRetransmitBurstHigh") {
		t.Errorf("GET report.md = %d %q", resp.StatusCode, string(body)[:min(80, len(body))])
	}

	resp, _ = http.Get(srv.URL + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
}

func TestRejectsBadPayloads(t *testing.T) {
	srv, _ := newTestServer(t)
	url := srv.URL + "/webhooks/alertmanager"
	if code, _ := post(t, url, `{not json`); code != http.StatusBadRequest {
		t.Errorf("invalid json = %d", code)
	}
	if code, _ := post(t, url, `{"version":"4","alerts":[]}`); code != http.StatusBadRequest {
		t.Errorf("empty alerts = %d", code)
	}
}

func TestAffectedNodesFallsBackToInstanceHost(t *testing.T) {
	srv, store := newTestServer(t)
	noNode := strings.ReplaceAll(firingPayload, `"node": "worker01",`, ``)
	_, created := post(t, srv.URL+"/webhooks/alertmanager", noNode)
	inc, err := store.Get(created.Incidents[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.AffectedNodes) != 1 || inc.AffectedNodes[0] != "192.168.0.18" {
		t.Errorf("affected_nodes = %v, want [192.168.0.18]", inc.AffectedNodes)
	}
}

type recordingInvestigator struct{ ids []string }

func (r *recordingInvestigator) Enqueue(id string, force bool) error {
	r.ids = append(r.ids, id)
	return nil
}

func TestWebhookEnqueuesOnlyNewFiringIncidents(t *testing.T) {
	store, _ := incident.NewFileStore(t.TempDir())
	inv := &recordingInvestigator{}
	srv := httptest.NewServer(New(Options{Store: store, Investigator: inv, AutoInvestigate: true}))
	defer srv.Close()
	url := srv.URL + "/webhooks/alertmanager"

	post(t, url, firingPayload) // new -> enqueue
	post(t, url, firingPayload) // redelivery -> no enqueue
	resolved := strings.ReplaceAll(firingPayload, `"status": "firing"`, `"status": "resolved"`)
	post(t, url, resolved) // resolved -> no enqueue
	if len(inv.ids) != 1 {
		t.Fatalf("enqueued %d times, want 1: %v", len(inv.ids), inv.ids)
	}
}

func TestListCause(t *testing.T) {
	cases := []struct {
		name string
		r    map[string]any
		want string
	}{
		{"nil result", nil, ""},
		{"root_cause only", map[string]any{"root_cause": "worker1 link congestion"}, "worker1 link congestion"},
		{"summary wins", map[string]any{
			"root_cause": "long paragraph",
			"summary":    map[string]any{"cause": "worker1 NIC 혼잡"},
		}, "worker1 NIC 혼잡"},
		{"empty summary falls back", map[string]any{
			"root_cause": "long paragraph",
			"summary":    map[string]any{"cause": ""},
		}, "long paragraph"},
	}
	for _, c := range cases {
		if got := listCause(c.r); got != c.want {
			t.Errorf("%s: listCause = %q, want %q", c.name, got, c.want)
		}
	}
}

// Group members retain their alert identity while reading the primary's analysis.
func TestDetailAndReportUseGroupAnalysis(t *testing.T) {
	srv, store := newTestServer(t)
	primary := &incident.Incident{IncidentID: "inc-111111111111", AlertStatus: "firing", HolmesStatus: "done", HolmesAttempts: 2,
		HolmesModel: "primary-model", HolmesTotalTokens: 431,
		HolmesResult:    map[string]any{"root_cause": "Shared link saturated", "investigation_status": "excluded", "confidence": "high"},
		EvidenceMetrics: []incident.Evidence{{Metric: "primary metric", Observation: "primary observation"}}}
	member := &incident.Incident{IncidentID: "inc-222222222222", AlertStatus: "resolved", RecoveryStatus: "resolved", CorrelationID: primary.IncidentID,
		SymptomSummary: "Member DNS symptom", AffectedNodes: []string{"member-node"}, StartsAt: time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)}
	for _, inc := range []*incident.Incident{primary, member} {
		if err := store.Save(inc); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get(srv.URL + "/incidents/inc-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	var detail struct {
		IncidentID  string `json:"incident_id"`
		AlertStatus string `json:"alert_status"`
		Analysis    struct {
			SourceIncidentID string         `json:"source_incident_id"`
			Shared           bool           `json:"shared"`
			Result           map[string]any `json:"result"`
		} `json:"analysis"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if detail.IncidentID != "inc-222222222222" || detail.AlertStatus != "resolved" || !detail.Analysis.Shared || detail.Analysis.SourceIncidentID != "inc-111111111111" || detail.Analysis.Result["root_cause"] != "Shared link saturated" {
		t.Errorf("detail must keep member identity and select group analysis: %+v", detail)
	}
	resp, err = http.Get(srv.URL + "/incidents/inc-222222222222/report.md")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"inc-222222222222", "resolved", "Member DNS symptom", "member-node", "Shared link saturated", "inc-111111111111", "excluded", "primary-model", "431", "primary observation"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("group report missing %q: %s", want, body)
		}
	}
	if strings.Contains(string(body), "**confirmed**") {
		t.Error("completed excluded result must not become confirmed")
	}
	// A member that receives its own manual investigation no longer borrows the primary.
	_, err = store.Update(member.IncidentID, func(i *incident.Incident) error {
		i.HolmesAttempts = 1
		i.HolmesStatus = "failed"
		i.HolmesResult = map[string]any{"root_cause": "Earlier member finding", "investigation_status": "inconclusive"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(srv.URL + "/incidents/inc-222222222222/report.md")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Previous analysis") || !strings.Contains(string(body), "Earlier member finding") || strings.Contains(string(body), "Shared link saturated") {
		t.Fatalf("failed retry must retain clearly labelled own previous analysis: %s", body)
	}
}

func TestWebhookEnrollmentIsDurableAndFrozenAcrossConcurrentRedeliveries(t *testing.T) {
	store, _ := incident.NewFileStore(t.TempDir())
	now := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	eligible := func(i *incident.Incident) bool { return i.RuleID == "rule-1" }
	h := New(Options{Store: store, SlackChannelID: "C-original", Eligible: eligible, Now: func() time.Time { return now }})
	payload := strings.ReplaceAll(firingPayload, "smoke-1", "rule-1")
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/alertmanager", strings.NewReader(payload)))
			if w.Code != http.StatusOK {
				t.Errorf("delivery status = %d", w.Code)
			}
		}()
	}
	wg.Wait()
	all, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].DeliveryCount != 12 {
		t.Fatalf("concurrent deliveries = %+v", all)
	}
	inc := all[0]
	if inc.SlackNotification == nil {
		t.Fatal("new eligible firing incident lacks durable enrollment")
	}
	if inc.SlackNotification.ChannelID != "C-original" || !inc.SlackNotification.EnrolledAt.Equal(now) || inc.SlackNotification.Parent.State != "pending" {
		t.Fatalf("unexpected durable enrollment: %+v", inc.SlackNotification)
	}
	// A configuration change cannot replace the frozen channel or receipt.
	_, err = store.Update(inc.IncidentID, func(i *incident.Incident) error {
		i.SlackNotification.Parent.State = "sent"
		i.SlackNotification.Parent.TS = "123.456"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	New(Options{Store: store, SlackChannelID: "C-new", Eligible: eligible}).ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/alertmanager", strings.NewReader(payload)))
	inc, _ = store.Get(inc.IncidentID)
	if inc.SlackNotification.ChannelID != "C-original" || inc.SlackNotification.Parent.TS != "123.456" || inc.SlackNotification.Parent.State != "sent" {
		t.Fatalf("redelivery replaced receipt: %+v", inc.SlackNotification)
	}
}

func TestWebhookEnrollmentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, rule, channel, status string
		eligible                    bool
	}{
		{"disabled", "rule-1", "", "firing", true},
		{"ineligible", "rule-1", "C1", "firing", false},
		{"smoke", "smoke-1", "C1", "firing", true},
		{"resolved first", "rule-1", "C1", "resolved", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := incident.NewFileStore(t.TempDir())
			payload := strings.ReplaceAll(firingPayload, "smoke-1", tc.rule)
			payload = strings.ReplaceAll(payload, `"status": "firing"`, `"status": "`+tc.status+`"`)
			w := httptest.NewRecorder()
			New(Options{Store: store, SlackChannelID: tc.channel, Eligible: func(*incident.Incident) bool { return tc.eligible }}).ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/alertmanager", strings.NewReader(payload)))
			all, _ := store.List()
			if len(all) != 1 || all[0].SlackNotification != nil {
				t.Fatalf("unexpected enrollment: %+v", all)
			}
			// Existing records, including resolved-first and pre-feature history,
			// never enroll when replayed after configuration becomes eligible.
			w = httptest.NewRecorder()
			New(Options{Store: store, SlackChannelID: "C-enabled", Eligible: func(*incident.Incident) bool { return true }}).ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/alertmanager", strings.NewReader(payload)))
			all, _ = store.List()
			if all[0].SlackNotification != nil {
				t.Fatal("historical incident enrolled on redelivery")
			}
		})
	}
}

func TestDetailHidesNotificationReceiptsAndLinksRequestedIncident(t *testing.T) {
	store, _ := incident.NewFileStore(t.TempDir())
	inc := &incident.Incident{IncidentID: "inc-333333333333", RuleID: "rule-1", StartsAt: time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC), AffectedNodes: []string{"own-node"},
		SlackNotification: &incident.SlackNotification{ChannelID: "private-channel", Parent: incident.NotificationDelivery{State: "sent", Payload: "private-payload", TS: "private-ts"}}}
	if err := store.Save(inc); err != nil {
		t.Fatal(err)
	}
	builder, err := links.New("https://grafana.example", "https://nd.example")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	New(Options{Store: store, Links: builder}).ServeHTTP(w, httptest.NewRequest("GET", "/incidents/"+inc.IncidentID, nil))
	for _, private := range []string{"slack_notification", "private-channel", "private-payload", "private-ts", `"Source"`} {
		if strings.Contains(w.Body.String(), private) {
			t.Errorf("detail leaks %s", private)
		}
	}
	var detail struct {
		Links links.Links `json:"links"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.Links.Grafana, "var-node=own-node") || detail.Links.Incident != "https://nd.example/#/incident/inc-333333333333" {
		t.Fatalf("wrong member links: %+v", detail.Links)
	}
	persisted, _ := store.Get(inc.IncidentID)
	if persisted.SlackNotification == nil || persisted.SlackNotification.Parent.TS != "private-ts" {
		t.Fatal("public projection altered persisted receipt")
	}
}
