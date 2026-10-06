package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"networkdoctor-agent/internal/backend/incident"
)

func TestJevRequestAndVerdict(t *testing.T) {
	var got jevRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{
			"consistent":{"type":"noul","noul":0.12},
			"supported_status":{"type":"choice","choice":"confirmed","probabilities":{"confirmed":0.8,"excluded":0.1,"inconclusive":0.1},"confidence":0.7}},
			"usage":{"input_tokens":512,"output_tokens":20}}`))
	}))
	defer srv.Close()

	j := NewJev(srv.URL, "", "k")
	in := Input{
		Labels:  map[string]string{"rule_id": "rule-4", "scenario": "coredns-degradation"},
		Derived: []incident.Evidence{{Metric: "derived:coredns_pod_p99_ratio", Observation: "3.9x"}},
		Result:  map[string]any{"investigation_status": "confirmed", "root_cause": "slow upstream"},
	}
	v, err := j.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Pass(0.5) || v.SupportedStatus != "confirmed" || v.InputTokens != 512 || v.Model != "jev-1.13.0" {
		t.Fatalf("verdict %+v", v)
	}
	if got.Model != "jev-latest" || got.Questions["consistent"].Type != "noul" || got.Questions["supported_status"].Type != "choice" {
		t.Fatalf("request %+v", got)
	}
	if fb := Feedback(in, v); !strings.Contains(fb, "coredns_pod_p99_ratio: 3.9x") || !strings.Contains(fb, "slow upstream") {
		t.Fatalf("feedback %s", fb)
	}
}

func TestNoKeyNoChecker(t *testing.T) {
	if NewJev("", "", "") != nil {
		t.Fatal("want nil checker without a key")
	}
}
