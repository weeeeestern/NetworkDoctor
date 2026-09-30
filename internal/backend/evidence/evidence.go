// Package evidence collects a fixed, cheap snapshot of NetworkDoctor eBPF
// signals around an alert. It does not decide anything: firing/resolved is
// Prometheus' job and root-cause reasoning is Holmes'. The snapshot is stored
// on the incident and handed to Holmes as a starting point.
package evidence

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/prometheus"
)

// Query is one evidence query. {F} is a reserved placeholder for a label
// filter; it is currently replaced by nothing (all nodes).
type Query struct {
	Name  string
	Expr  string
	Unit  string
	Scale float64 // multiply values before formatting (1 = as is)
}

// DefaultQueries are node-level eBPF signals every NetworkDoctor scenario
// cares about. All ebpf_* series carry a `node` label (ServiceMonitor
// relabeling), so the alert's node can scope them.
var DefaultQueries = []Query{
	{Name: "tcp_retransmits_per_s", Expr: `sum by (node) (rate(ebpf_tcp_retransmits_total{F}[2m]))`, Unit: "/s", Scale: 1},
	{Name: "tcp_retransmit_bursts_per_s", Expr: `sum by (node) (rate(ebpf_tcp_retransmit_bursts_total{F}[2m]))`, Unit: "/s", Scale: 1},
	{Name: "tcp_connect_fail_ratio", Expr: `sum by (node) (rate(ebpf_tcp_connect_failed_total{F}[2m])) / clamp_min(sum by (node) (rate(ebpf_tcp_connect_attempts_total{F}[2m])), 0.001)`, Unit: "", Scale: 1},
	{Name: "tcp_srtt_p99_ms", Expr: `histogram_quantile(0.99, sum by (le, node) (rate(ebpf_tcp_srtt_microseconds_bucket{F}[2m])))`, Unit: "ms", Scale: 0.001},
	{Name: "runqlat_p99_ms", Expr: `histogram_quantile(0.99, sum by (le, node) (rate(ebpf_runqlat_bucket{F}[2m])))`, Unit: "ms", Scale: 0.001},
	{Name: "dns_slow_per_s", Expr: `sum by (node) (rate(ebpf_dns_slow_total{F}[2m]))`, Unit: "/s", Scale: 1},
}

// Collector runs the evidence queries.
type Collector struct {
	Prom    *prometheus.Client
	Queries []Query
	Step    time.Duration
}

// Collect queries [start, end] across all nodes so the alert node can be
// compared with its peers; the alert node is listed first.
func (c *Collector) Collect(ctx context.Context, node string, start, end time.Time) []incident.Evidence {
	queries := c.Queries
	if len(queries) == 0 {
		queries = DefaultQueries
	}
	step := c.Step
	if step <= 0 {
		step = 30 * time.Second
	}

	out := make([]incident.Evidence, 0, len(queries))
	for _, q := range queries {
		expr := strings.ReplaceAll(q.Expr, "{F}", "")
		series, err := c.Prom.QueryRange(ctx, expr, start, end, step)
		ev := incident.Evidence{Metric: q.Name, Query: expr, Source: "networkdoctor-backend"}
		switch {
		case err != nil:
			ev.Observation = "query failed: " + err.Error()
		case len(series) == 0:
			ev.Observation = "no data in window"
		default:
			ev.Observation = summarize(series, node, q)
		}
		out = append(out, ev)
	}
	return out
}

// summarize reports peak and mean for the alert node first, then peers.
func summarize(series []prometheus.Series, node string, q Query) string {
	sort.SliceStable(series, func(i, j int) bool {
		ai, aj := series[i].Labels["node"] == node, series[j].Labels["node"] == node
		if ai != aj {
			return ai
		}
		return series[i].Labels["node"] < series[j].Labels["node"]
	})
	parts := make([]string, 0, len(series))
	for _, s := range series {
		if len(s.Samples) == 0 {
			continue
		}
		var sum, peak float64
		var peakAt time.Time
		for i, p := range s.Samples {
			v := p.V * q.Scale
			sum += v
			if i == 0 || v > peak {
				peak, peakAt = v, p.T
			}
		}
		mean := sum / float64(len(s.Samples))
		name := s.Labels["node"]
		if name == "" {
			name = "all"
		}
		mark := ""
		if node != "" && s.Labels["node"] == node {
			mark = " (alert node)"
		}
		parts = append(parts, fmt.Sprintf("%s%s: peak %s%s at %s, mean %s%s",
			shortNode(name), mark, fmtNum(peak), q.Unit, peakAt.Format("15:04:05Z"), fmtNum(mean), q.Unit))
	}
	if len(parts) == 0 {
		return "no data in window"
	}
	return strings.Join(parts, "; ")
}

func shortNode(n string) string {
	if i := strings.Index(n, "-"); i > 0 && strings.HasPrefix(n, "worker") {
		return n[:i]
	}
	return n
}

func fmtNum(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.2f", v)
	default:
		return fmt.Sprintf("%.3f", v)
	}
}
