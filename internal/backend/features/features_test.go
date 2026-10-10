package features

import (
	"context"
	"strings"
	"testing"
	"time"

	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/prometheus"
)

type fakeProm map[string][]prometheus.Series // keyed by a substring of the query

// QueryRange returns the series of the longest key contained in the query,
// so overlapping keys resolve deterministically.
func (f fakeProm) QueryRange(_ context.Context, q string, _, _ time.Time, _ time.Duration) ([]prometheus.Series, error) {
	best := ""
	for k := range f {
		if strings.Contains(q, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return nil, nil
	}
	return f[best], nil
}

var t0 = time.Date(2026, 10, 2, 5, 30, 0, 0, time.UTC)

func ser(labels map[string]string, vals ...float64) prometheus.Series {
	s := prometheus.Series{Labels: labels}
	for i, v := range vals {
		s.Samples = append(s.Samples, prometheus.Sample{T: t0.Add(time.Duration(i) * time.Minute), V: v})
	}
	return s
}

func TestCoreDNSRatioPointsAtSlowPod(t *testing.T) {
	p := fakeProm{
		"coredns_dns_request_duration_seconds_bucket": {
			ser(map[string]string{"pod": "coredns-a"}, 0.01, 0.96, 0.98),
			ser(map[string]string{"pod": "coredns-b"}, 0.01, 0.24, 0.25),
		},
		"coredns_dns_requests_total": {
			ser(map[string]string{"pod": "coredns-a"}, 1, 6.5),
			ser(map[string]string{"pod": "coredns-b"}, 1, 7.0),
		},
		"kube_pod_info": {
			ser(map[string]string{"pod": "coredns-a", "node": "worker02-x"}, 1),
			ser(map[string]string{"pod": "coredns-b", "node": "worker01-x"}, 1),
		},
	}
	d := &Deriver{Prom: p}
	inc := &incident.Incident{Scenario: "coredns-degradation"}
	got := d.Derive(context.Background(), inc, nil, t0, t0.Add(10*time.Minute))
	if len(got) != 2 {
		t.Fatalf("want 2 facts, got %d: %+v", len(got), got)
	}
	r := got[1].Observation
	if !strings.Contains(r, "3.9x") || !strings.Contains(r, "local to coredns-a on node worker02-x") {
		t.Fatalf("ratio fact: %s", r)
	}
}

func TestCoreDNSEvenSlowdownReadsAsShared(t *testing.T) {
	p := fakeProm{
		"coredns_dns_request_duration_seconds_bucket": {
			ser(map[string]string{"pod": "coredns-a"}, 0.9),
			ser(map[string]string{"pod": "coredns-b"}, 0.8),
		},
		"coredns_dns_requests_total": {
			ser(map[string]string{"pod": "coredns-a"}, 5),
			ser(map[string]string{"pod": "coredns-b"}, 5),
		},
	}
	got := (&Deriver{Prom: p}).Derive(context.Background(), &incident.Incident{Scenario: "coredns-degradation"}, nil, t0, t0)
	if !strings.Contains(got[1].Observation, "shared or upstream") {
		t.Fatalf("got %s", got[1].Observation)
	}
}

func TestCongestionTimingAndCandidatePods(t *testing.T) {
	created := float64(t0.Add(-1 * time.Minute).Unix())
	p := fakeProm{
		"http_request_duration_seconds_bucket": {ser(nil, 0.1, 2.4, 2.45, 2.44, 0.1)},
		"ebpf_tcp_retransmits_total": {
			ser(map[string]string{"node": "worker01-x"}, 0, 0, 0, 0, 0),
			ser(map[string]string{"node": "worker02-x"}, 0, 5.3, 5.6, 5.1, 0),
		},
		"kube_pod_created": {ser(map[string]string{"namespace": "demo", "pod": "nd-r1-client"}, created)},
	}
	inc := &incident.Incident{Scenario: "network-congestion", AlertLabels: map[string]string{
		"target_namespace": "demo", "service": "catshop", "variant": "cross-node", "retransmit_node": "worker02-x"}}
	got := (&Deriver{Prom: p}).Derive(context.Background(), inc, nil, t0.Add(-5*time.Minute), t0.Add(10*time.Minute))
	if len(got) != 2 {
		t.Fatalf("want 2 facts, got %+v", got)
	}
	if !strings.Contains(got[0].Observation, "Onset lag (latency minus retransmits) 0s") || !strings.Contains(got[0].Observation, "supports a link") {
		t.Fatalf("timing: %s", got[0].Observation)
	}
	if !strings.Contains(got[1].Observation, "demo/nd-r1-client") {
		t.Fatalf("pods: %s", got[1].Observation)
	}
}

func TestConntrackLimitChangeDetected(t *testing.T) {
	p := fakeProm{"node_nf_conntrack_entries_limit": {
		ser(map[string]string{"node": "worker02-x"}, 131072, 560, 540, 131072),
		ser(map[string]string{"node": "worker01-x"}, 131072, 131072),
	}}
	inc := &incident.Incident{Scenario: "conntrack-exhaustion"}
	rel := []*incident.Incident{{Scenario: "dns-conntrack-correlation"}}
	got := (&Deriver{Prom: p}).Derive(context.Background(), inc, rel, t0, t0)
	if len(got) != 1 { // shared between the two scenarios
		t.Fatalf("want 1 fact, got %d", len(got))
	}
	o := got[0].Observation
	if !strings.Contains(o, "CHANGED") || !strings.Contains(o, "worker02-x: 131072 at window start, min 540") || !strings.Contains(o, "worker01-x 131072") {
		t.Fatalf("got %s", o)
	}
}

func TestMissingDataIsSaidOutLoud(t *testing.T) {
	got := (&Deriver{Prom: fakeProm{}}).Derive(context.Background(), &incident.Incident{Scenario: "coredns-degradation"}, nil, t0, t0)
	if len(got) != 1 || !strings.HasPrefix(got[0].Observation, "unavailable") {
		t.Fatalf("got %+v", got)
	}
}

func TestNodeLocalizedRefusalsAndCandidatePod(t *testing.T) {
	created := float64(t0.Add(-30 * time.Second).Unix())
	p := fakeProm{
		"sum by (node) (rate(ebpf_tcp_connect_failed_total[1m])) / clamp_min": {
			ser(map[string]string{"node": "worker02-x"}, 0.01, 0.7, 0.71, 0.02),
			ser(map[string]string{"node": "worker01-x"}, 0.01, 0.02, 0.01, 0.01),
		},
		"sum by (node) (rate(ebpf_tcp_connect_failed_total[1m]))": {ser(map[string]string{"node": "worker02-x"}, 0, 7.5, 7.6, 0)},
		"ebpf_tcp_retransmits_total":                              {ser(map[string]string{"node": "worker02-x"}, 0, 0.01, 0, 0)},
		"kube_pod_created":                                        {ser(map[string]string{"namespace": "demo", "pod": "nd-r6-client"}, created)},
	}
	inc := &incident.Incident{Scenario: "node-localized-failure", StartsAt: t0.Add(5 * time.Minute),
		AlertLabels: map[string]string{"node": "worker02-x"}}
	got := (&Deriver{Prom: p}).Derive(context.Background(), inc, nil, t0.Add(-10*time.Minute), t0.Add(10*time.Minute))
	if len(got) != 3 {
		t.Fatalf("want 3 facts, got %+v", got)
	}
	if !strings.Contains(got[0].Observation, "peak 71%") || !strings.Contains(got[0].Observation, "other node 2%") {
		t.Fatalf("profile: %s", got[0].Observation)
	}
	if !strings.Contains(got[1].Observation, "actively refused or reset") {
		t.Fatalf("signature: %s", got[1].Observation)
	}
	if !strings.Contains(got[2].Observation, "demo/nd-r6-client") || !strings.Contains(got[2].Observation, "1m30s before failures began") {
		t.Fatalf("pods: %s", got[2].Observation)
	}
}
