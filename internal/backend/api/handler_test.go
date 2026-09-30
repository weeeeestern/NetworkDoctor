package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"networkdoctor-agent/internal/backend/incident"
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
