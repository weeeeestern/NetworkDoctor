package links

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"networkdoctor-agent/internal/backend/incident"
)

func TestLinksPreserveSubpathEncodeLiteralLabelsAndWindow(t *testing.T) {
	b, err := New("https://dash.example/grafana/", "https://incidents.example/")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	inc := &incident.Incident{IncidentID: "inc-one", RuleID: "rule-1", StartsAt: start, EndsAt: &end, AffectedNodes: []string{"worker +&/a", "worker-b"}, AffectedServices: []string{"demo/wrong"}, AlertLabels: map[string]string{"retransmit_node": "worker-c", "service": "cart +&/api"}, GeneratorURL: "http://prometheus.internal", ExternalURL: "http://alertmanager.internal"}
	got := b.For(inc)
	u, err := url.Parse(got.Grafana)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "dash.example" || u.Path != "/grafana/d/nd-rule-1" {
		t.Fatalf("Grafana URL = %q", got.Grafana)
	}
	q := u.Query()
	if q.Get("from") != "1794185400000" || q.Get("to") != "1794186900000" {
		t.Fatalf("window = %v", q)
	}
	if !reflect.DeepEqual(q["var-node"], []string{"worker +&/a", "worker-b", "worker-c"}) || q.Get("var-service") != "cart +&/api" {
		t.Fatalf("variables = %v", q)
	}
	if got.Incident != "https://incidents.example/#/incident/inc-one" {
		t.Fatalf("incident URL = %q", got.Incident)
	}
	if strings.Contains(got.Grafana, "internal") || strings.Contains(got.Incident, "internal") {
		t.Fatal("used internal alert URLs")
	}
}

func TestRuleSpecificNodesAndRawService(t *testing.T) {
	for _, tc := range []struct {
		rule  string
		nodes []string
	}{
		{"rule-1", []string{"a", "b"}},
		{"rule-2", []string{"a"}},
		{"rule-6", nil},
		{"rule-8", []string{"a"}},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			b, err := New("http://dash.example", "")
			if err != nil {
				t.Fatal(err)
			}
			got := b.For(&incident.Incident{RuleID: tc.rule, AffectedNodes: []string{"a", "a"}, AffectedServices: []string{"ns/service"}, AlertLabels: map[string]string{"retransmit_node": "b"}})
			u, _ := url.Parse(got.Grafana)
			q := u.Query()
			if !reflect.DeepEqual(q["var-node"], tc.nodes) {
				t.Fatalf("nodes = %v, want %v", q["var-node"], tc.nodes)
			}
			if _, ok := q["var-service"]; ok {
				t.Fatalf("invented service variable: %v", q)
			}
			if q.Get("to") != "now" {
				t.Fatalf("open end = %v", q)
			}
		})
	}
}

func TestLinksHideUnconfiguredAndUnknownDashboards(t *testing.T) {
	b, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.For(&incident.Incident{RuleID: "rule-1", IncidentID: "a", GeneratorURL: "https://internal", ExternalURL: "https://internal"}); got != (Links{}) {
		t.Fatalf("unconfigured links = %+v", got)
	}
	b, err = New("https://grafana.example", "https://ui.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"", "rule-0", "rule-9", "rule-01", "smoke-1"} {
		got := b.For(&incident.Incident{RuleID: rule, IncidentID: "a"})
		if got.Grafana != "" || got.Incident != "https://ui.example/#/incident/a" {
			t.Fatalf("unknown rule %q links = %+v", rule, got)
		}
	}
	if got := b.For(nil); got != (Links{}) {
		t.Fatalf("nil links = %+v", got)
	}
}

func TestNewRejectsUnsafeOrAmbiguousBases(t *testing.T) {
	for _, base := range []string{"ftp://example.com", "javascript:alert(1)", "//example.com", "https:///path", "https://user:pass@example.com", "https://example.com?x=1", "https://example.com#fragment", "https://example.com?", "https://example.com/#"} {
		for _, kind := range []string{"grafana", "public"} {
			t.Run(kind+"/"+base, func(t *testing.T) {
				var err error
				if kind == "grafana" {
					_, err = New(base, "")
				} else {
					_, err = New("", base)
				}
				if err == nil {
					t.Fatalf("accepted %s base %q", kind, base)
				}
			})
		}
	}
	for _, base := range []string{"https://example.com/app", "https://example.com/%2F"} {
		if _, err := New("", base); err == nil {
			t.Fatalf("accepted public asset subpath %q", base)
		}
	}
}

func TestInstanceIPFallbackIsNotANodeFilter(t *testing.T) {
	b, err := New("https://dash.example", "")
	if err != nil {
		t.Fatal(err)
	}
	inc := &incident.Incident{RuleID: "rule-2", StartsAt: time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC), AffectedNodes: []string{"192.168.0.18"}}
	u, err := url.Parse(b.For(inc).Grafana)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query()["var-node"]; len(got) != 0 {
		t.Fatalf("var-node = %v, want none for an instance IP", got)
	}
}
