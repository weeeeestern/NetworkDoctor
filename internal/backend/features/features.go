// Package features computes deterministic, scenario-specific facts for an
// incident ("derived facts"): numbers the LLM tends to misread when it has to
// work them out itself, such as the latency ratio between CoreDNS pods, which
// of two signals rose first, whether a kernel limit was changed, or whether
// failed connects are refusals or drops.
//
// Facts are computed in code from PromQL and handed to Holmes as evidence.
// They state measurements and the threshold readings the skills define; the
// root-cause decision stays with Holmes. The "derived" architecture enables
// them; "baseline" does not.
package features

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"networkdoctor-agent/internal/backend/incident"
	"networkdoctor-agent/internal/backend/prometheus"
)

// Querier is the subset of prometheus.Client used here.
type Querier interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]prometheus.Series, error)
}

// Deriver computes derived facts.
type Deriver struct {
	Prom Querier
	Step time.Duration // default 30s
}

// Derive returns facts for every scenario present in the incident and its
// correlated members. Unknown scenarios yield nothing. Query failures become
// a fact that says so, so a missing number is never mistaken for a normal one.
func (d *Deriver) Derive(ctx context.Context, inc *incident.Incident, related []*incident.Incident, start, end time.Time) []incident.Evidence {
	if d == nil || d.Prom == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []incident.Evidence
	for _, i := range append([]*incident.Incident{inc}, related...) {
		key := i.Scenario + "|" + i.AlertLabels["variant"]
		if seen[key] {
			continue
		}
		seen[key] = true
		switch i.Scenario {
		case "coredns-degradation":
			out = append(out, d.coreDNS(ctx, start, end)...)
		case "network-congestion":
			out = append(out, d.congestion(ctx, i, start, end)...)
		case "node-localized-failure":
			out = append(out, d.nodeLocalized(ctx, i, start, end)...)
		case "conntrack-exhaustion", "dns-conntrack-correlation":
			if !seen["conntrack-limit"] {
				seen["conntrack-limit"] = true
				out = append(out, d.conntrackLimit(ctx, start, end)...)
			}
		}
	}
	return out
}

func (d *Deriver) step() time.Duration {
	if d.Step > 0 {
		return d.Step
	}
	return 30 * time.Second
}

func (d *Deriver) query(ctx context.Context, q string, start, end time.Time) ([]prometheus.Series, error) {
	return d.Prom.QueryRange(ctx, q, start, end, d.step())
}

func fact(name, query, obs string) incident.Evidence {
	return incident.Evidence{Metric: "derived:" + name, Query: query, Observation: obs, Source: "networkdoctor-backend/derived"}
}

// ---------------------------------------------------------------- CoreDNS

const (
	qCoreDNSP99  = `histogram_quantile(0.99, sum by (le, pod) (rate(coredns_dns_request_duration_seconds_bucket[1m])))`
	qCoreDNSReq  = `sum by (pod) (rate(coredns_dns_requests_total[1m]))`
	qCoreDNSNode = `max by (pod, node) (kube_pod_info{namespace="kube-system", pod=~"coredns.*"})`
)

func (d *Deriver) coreDNS(ctx context.Context, start, end time.Time) []incident.Evidence {
	p99, err := d.query(ctx, qCoreDNSP99, start, end)
	if err != nil || len(p99) == 0 {
		return []incident.Evidence{fact("coredns_pod_p99_ratio", qCoreDNSP99, unavailable(err))}
	}
	req, _ := d.query(ctx, qCoreDNSReq, start, end)
	nodes := map[string]string{}
	if s, err := d.query(ctx, qCoreDNSNode, start, end); err == nil {
		for _, x := range s {
			nodes[x.Labels["pod"]] = shortNode(x.Labels["node"])
		}
	}
	reqPeak := map[string]float64{}
	for _, s := range req {
		reqPeak[s.Labels["pod"]], _ = peak(s.Samples)
	}

	type row struct {
		pod  string
		p99  float64
		at   time.Time
		reqs float64
	}
	var rows []row
	for _, s := range p99 {
		v, at := peak(s.Samples)
		if math.IsNaN(v) {
			continue
		}
		rows = append(rows, row{s.Labels["pod"], v, at, reqPeak[s.Labels["pod"]]})
	}
	if len(rows) < 2 {
		return []incident.Evidence{fact("coredns_pod_p99_ratio", qCoreDNSP99,
			fmt.Sprintf("only %d CoreDNS pod(s) had latency data; no peer to compare", len(rows)))}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].p99 > rows[j].p99 })
	slow, fast := rows[0], rows[len(rows)-1]

	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s (node %s) p99 peak %.3fs at %s, request peak %.2f/s",
			r.pod, orDash(nodes[r.pod]), r.p99, r.at.Format("15:04:05Z"), r.reqs))
	}
	ratio := ratioOf(slow.p99, fast.p99)
	reqRatio := ratioOf(slow.reqs, fast.reqs)
	reading := "between 1.5 and 2: keep both a single-pod and a shared/upstream cause open"
	switch {
	case ratio >= 2 && reqRatio >= 0.67 && reqRatio <= 1.5:
		reading = fmt.Sprintf("ratio >= 2 at similar load: points to a cause local to %s on node %s, not upstream", slow.pod, orDash(nodes[slow.pod]))
	case ratio >= 2:
		reading = "ratio >= 2 but load differs by more than 1.5x: check whether the slow pod simply received more traffic"
	case ratio < 1.5:
		reading = "ratio < 1.5: all pods slowed alike, consistent with a shared or upstream cause"
	}
	return []incident.Evidence{
		fact("coredns_pod_p99", qCoreDNSP99, strings.Join(parts, "; ")),
		fact("coredns_pod_p99_ratio", qCoreDNSP99, fmt.Sprintf(
			"slowest/fastest p99 = %.3fs / %.3fs = %.1fx; request-rate ratio %.2f. Reading: %s",
			slow.p99, fast.p99, ratio, reqRatio, reading)),
	}
}

// ------------------------------------------------------------- congestion

func (d *Deriver) congestion(ctx context.Context, inc *incident.Incident, start, end time.Time) []incident.Evidence {
	l := inc.AlertLabels
	ns := firstNonEmpty(l["target_namespace"], l["namespace"])
	svc := l["service"]
	if svc == "" {
		return nil
	}
	qLat := fmt.Sprintf(`histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{namespace=%q, service=%q}[1m])))`, ns, svc)
	qRet := `sum by (node) (rate(ebpf_tcp_retransmits_total[1m]))`

	lat, err := d.query(ctx, qLat, start, end)
	if err != nil || len(lat) == 0 {
		return []incident.Evidence{fact("latency_retransmit_timing", qLat, unavailable(err))}
	}
	ret, err := d.query(ctx, qRet, start, end)
	if err != nil || len(ret) == 0 {
		return []incident.Evidence{fact("latency_retransmit_timing", qRet, unavailable(err))}
	}

	node := firstNonEmpty(l["retransmit_node"], l["node"])
	var rs *prometheus.Series
	for i := range ret {
		if node != "" && ret[i].Labels["node"] == node {
			rs = &ret[i]
		}
	}
	if rs == nil { // fall back to the node with the highest retransmit peak
		best := -1.0
		for i := range ret {
			if v, _ := peak(ret[i].Samples); v > best {
				best, rs = v, &ret[i]
			}
		}
		node = rs.Labels["node"]
	}

	latOn, latOff := episode(lat[0].Samples, 0.5)
	retOn, retOff := episode(rs.Samples, 0.2)
	corr := correlation(lat[0].Samples, rs.Samples)

	var obs strings.Builder
	fmt.Fprintf(&obs, "%s/%s p99 > 0.5s: %s; retransmits on %s > 0.2/s: %s. ", ns, svc,
		span(latOn, latOff), shortNode(node), span(retOn, retOff))
	if !latOn.IsZero() && !retOn.IsZero() {
		lag := latOn.Sub(retOn).Round(time.Second)
		fmt.Fprintf(&obs, "Onset lag (latency minus retransmits) %s. ", lag)
	}
	fmt.Fprintf(&obs, "Pearson correlation of the two 1m series over the window: %.2f. ", corr)
	switch {
	case !latOn.IsZero() && !retOn.IsZero() && absDur(latOn.Sub(retOn)) <= 2*time.Minute && corr >= 0.6:
		obs.WriteString("Reading: onsets within 2m and strongly correlated, supports a link.")
	case !latOn.IsZero() && !retOn.IsZero() && absDur(latOn.Sub(retOn)) > 5*time.Minute:
		obs.WriteString("Reading: onsets more than 5m apart, argues against a link.")
	default:
		obs.WriteString("Reading: timing alone neither supports nor rules out a link.")
	}
	out := []incident.Evidence{fact("latency_retransmit_timing", qLat+"  |  "+qRet, obs.String())}

	// Pods that started on the retransmitting node before the alert are
	// candidate clients (kube-state-metrics keeps them after deletion).
	if f, ok := d.podsStarted(ctx, "pods_started_on_retransmit_node", node, inc, start, end, time.Time{}); ok {
		out = append(out, f)
	}
	return out
}

// --------------------------------------------------------------- conntrack

const qConntrackLimit = `max by (node) (node_nf_conntrack_entries_limit * on (instance) group_left (node) max by (instance, node) (label_replace(kube_node_info, "instance", "$1:9100", "internal_ip", "(.*)")))`

func (d *Deriver) conntrackLimit(ctx context.Context, start, end time.Time) []incident.Evidence {
	s, err := d.query(ctx, qConntrackLimit, start, end)
	if err != nil || len(s) == 0 {
		return []incident.Evidence{fact("conntrack_limit_changes", qConntrackLimit, unavailable(err))}
	}
	var changed, steady []string
	for _, x := range s {
		lo, hi, first, last := math.Inf(1), math.Inf(-1), math.NaN(), math.NaN()
		for _, p := range x.Samples {
			if math.IsNaN(p.V) {
				continue
			}
			if math.IsNaN(first) {
				first = p.V
			}
			last = p.V
			lo, hi = math.Min(lo, p.V), math.Max(hi, p.V)
		}
		if math.IsInf(lo, 0) {
			continue
		}
		n := shortNode(x.Labels["node"])
		if lo != hi {
			changed = append(changed, fmt.Sprintf("%s: %.0f at window start, min %.0f, max %.0f, %.0f at window end", n, first, lo, hi, last))
		} else {
			steady = append(steady, fmt.Sprintf("%s %.0f", n, lo))
		}
	}
	sort.Strings(changed)
	sort.Strings(steady)
	obs := "nf_conntrack_entries_limit did not change on any node (" + strings.Join(steady, ", ") + ")."
	if len(changed) > 0 {
		obs = "nf_conntrack_entries_limit CHANGED during the window on " + strings.Join(changed, "; ") +
			". Unchanged: " + orDash(strings.Join(steady, ", ")) +
			". A limit that drops during an incident is a configuration change (sysctl), not organic table growth."
	}
	return []incident.Evidence{fact("conntrack_limit_changes", qConntrackLimit, obs)}
}

// podsStarted lists pods created on node between the window start and the
// alert (pods created after the alert cannot have caused it). If ref is set,
// each pod also shows how long before ref it was created.
func (d *Deriver) podsStarted(ctx context.Context, name, node string, inc *incident.Incident, start, end, ref time.Time) (incident.Evidence, bool) {
	upper := end
	if !inc.StartsAt.IsZero() && inc.StartsAt.Before(end) {
		upper = inc.StartsAt
	}
	q := fmt.Sprintf(`max by (namespace, pod) (kube_pod_created * on (namespace, pod) group_left () kube_pod_info{node=%q})`, node)
	s, err := d.query(ctx, q, start, end)
	if err != nil {
		return fact(name, q, unavailable(err)), true
	}
	var names []string
	for _, x := range s {
		// A pod name can be reused (deleted and created again), so check
		// every distinct creation time in the window, not just the latest.
		seen := map[int64]bool{}
		for _, smp := range x.Samples {
			if math.IsNaN(smp.V) || seen[int64(smp.V)] {
				continue
			}
			seen[int64(smp.V)] = true
			created := time.Unix(int64(smp.V), 0).UTC()
			if created.Before(start) || created.After(upper) {
				continue
			}
			n := fmt.Sprintf("%s/%s (created %s", x.Labels["namespace"], x.Labels["pod"], created.Format("15:04:05Z"))
			if !ref.IsZero() {
				if lead := ref.Sub(created).Round(time.Second); lead >= 0 {
					n += fmt.Sprintf(", %s before failures began", lead)
				} else {
					n += fmt.Sprintf(", %s after failures began", -lead)
				}
			}
			names = append(names, n+")")
		}
	}
	sort.Strings(names)
	o := "none"
	if len(names) > 0 {
		o = strings.Join(names, ", ")
	}
	return fact(name, q, fmt.Sprintf("pods created on %s between the window start and the alert: %s", shortNode(node), o)), true
}

// ------------------------------------------------------- node-localized

const (
	qConnFailRatio = `sum by (node) (rate(ebpf_tcp_connect_failed_total[1m])) / clamp_min(sum by (node) (rate(ebpf_tcp_connect_attempts_total[1m])), 0.001)`
	qConnFail      = `sum by (node) (rate(ebpf_tcp_connect_failed_total[1m]))`
	qRetrans1m     = `sum by (node) (rate(ebpf_tcp_retransmits_total[1m]))`
)

// nodeLocalized describes Rule 6: which node fails, since when, whether the
// failures look like refusals (no packet loss) or loss, and which pods
// started on that node just before.
func (d *Deriver) nodeLocalized(ctx context.Context, inc *incident.Incident, start, end time.Time) []incident.Evidence {
	ratio, err := d.query(ctx, qConnFailRatio, start, end)
	if err != nil || len(ratio) == 0 {
		return []incident.Evidence{fact("connect_failure_profile", qConnFailRatio, unavailable(err))}
	}
	node := inc.AlertLabels["node"]
	var fs *prometheus.Series
	for i := range ratio {
		if node != "" && ratio[i].Labels["node"] == node {
			fs = &ratio[i]
		}
	}
	if fs == nil {
		best := -1.0
		for i := range ratio {
			if v, _ := peak(ratio[i].Samples); v > best {
				best, fs = v, &ratio[i]
			}
		}
		node = fs.Labels["node"]
	}
	rPeak, rAt := peak(fs.Samples)
	on, off := episode(fs.Samples, 0.1)
	others := 0.0
	for i := range ratio {
		if ratio[i].Labels["node"] != node {
			if v, _ := peak(ratio[i].Samples); v > others {
				others = v
			}
		}
	}
	out := []incident.Evidence{fact("connect_failure_profile", qConnFailRatio, fmt.Sprintf(
		"%s connect failure ratio peak %.0f%% at %s, above 10%% %s; highest on any other node %.0f%%.",
		shortNode(node), 100*rPeak, rAt.Format("15:04:05Z"), span(on, off), 100*others))}

	// Failures without retransmits mean connections are refused or reset at
	// the destination, not lost on the way.
	fail, _ := d.query(ctx, qConnFail, start, end)
	ret, _ := d.query(ctx, qRetrans1m, start, end)
	fPeak, rtPeak := pick(fail, node), pick(ret, node)
	reading := "Reading: failures come with retransmits — SYNs are being dropped, either by packet loss on the path or by a policy that drops rather than rejects (check NetworkPolicy/Hubble drops)."
	switch {
	case math.IsNaN(fPeak) || math.IsNaN(rtPeak):
		reading = "Reading: unavailable (missing series)."
	case fPeak >= 1 && rtPeak < 0.2:
		reading = "Reading: many failed connects but almost no retransmits — the connections are actively refused or reset (destination port not listening, or a reject rule), not dropped by the network or the NIC."
	}
	out = append(out, fact("connect_failure_signature", qConnFail+"  |  "+qRetrans1m, fmt.Sprintf(
		"on %s: failed connects peak %s/s, TCP retransmits peak %s/s. %s", shortNode(node), num(fPeak), num(rtPeak), reading)))

	if f, ok := d.podsStarted(ctx, "pods_started_on_failing_node", node, inc, start, end, on); ok {
		out = append(out, f)
	}
	return out
}

// pick returns the peak of the series for node (NaN if absent).
func pick(ss []prometheus.Series, node string) float64 {
	for _, s := range ss {
		if s.Labels["node"] == node {
			v, _ := peak(s.Samples)
			return v
		}
	}
	return math.NaN()
}

func num(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", v)
}

// ----------------------------------------------------------------- helpers

func unavailable(err error) string {
	if err != nil {
		return "unavailable: query failed: " + err.Error()
	}
	return "unavailable: no data in window (missing data, not a normal reading)"
}

// peak returns the largest non-NaN value and when it occurred (NaN if none).
func peak(ss []prometheus.Sample) (float64, time.Time) {
	v, at := math.NaN(), time.Time{}
	for _, p := range ss {
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
			continue
		}
		if math.IsNaN(v) || p.V > v {
			v, at = p.V, p.T
		}
	}
	return v, at
}

// episode returns the first and last sample time above threshold.
func episode(ss []prometheus.Sample, threshold float64) (on, off time.Time) {
	for _, p := range ss {
		if !math.IsNaN(p.V) && p.V > threshold {
			if on.IsZero() {
				on = p.T
			}
			off = p.T
		}
	}
	return on, off
}

// correlation is the Pearson coefficient over the union of timestamps.
// The Prometheus client drops NaN samples (e.g. a quantile with no traffic),
// so a missing point counts as 0.
func correlation(a, b []prometheus.Sample) float64 {
	av, bv := map[int64]float64{}, map[int64]float64{}
	for _, p := range a {
		av[p.T.Unix()] = zeroNaN(p.V)
	}
	for _, p := range b {
		bv[p.T.Unix()] = zeroNaN(p.V)
	}
	keys := map[int64]bool{}
	for k := range av {
		keys[k] = true
	}
	for k := range bv {
		keys[k] = true
	}
	var xs, ys []float64
	for k := range keys {
		xs = append(xs, av[k])
		ys = append(ys, bv[k])
	}
	n := float64(len(xs))
	if n < 3 {
		return math.NaN()
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx, my = mx/n, my/n
	var sxy, sxx, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0
	}
	return sxy / math.Sqrt(sxx*syy)
}

func zeroNaN(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func ratioOf(a, b float64) float64 {
	if b <= 0 || math.IsNaN(a) || math.IsNaN(b) {
		return math.Inf(1)
	}
	return a / b
}

func span(on, off time.Time) string {
	if on.IsZero() {
		return "never above threshold in window"
	}
	return fmt.Sprintf("from %s to %s", on.Format("15:04:05Z"), off.Format("15:04:05Z"))
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// shortNode keeps full node names: Holmes queries with them verbatim.
func shortNode(n string) string { return n }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
