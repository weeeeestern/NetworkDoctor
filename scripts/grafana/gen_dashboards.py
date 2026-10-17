#!/usr/bin/env python3
"""Generate the NetworkDoctor Grafana dashboards.

Source of truth for deploy/helm/networkdoctor/dashboards/*.json — edit this
script and re-run it; never edit the JSON by hand.

    python3 scripts/grafana/gen_dashboards.py

Produces one overview dashboard (always-on, layered L7 -> CNI map) and one
incident dashboard per scenario rule (rule-1 .. rule-8), each with three rows
mirroring the skill structure:

    Trigger    — the exact signals the PrometheusRule fires on, with the rule
                 threshold drawn as a line
    Supporting — independent signals the matching Holmes skill uses to confirm
    Exclusion  — the alternative causes the skill rules out

Metric constraints (verified on the lab cluster, see
deploy/helm/networkdoctor/rules/networkdoctor-scenario-rules.yaml):
  - ebpf_* is per node only; node_* and hubble_* get a node label by joining
    on kube_node_info.internal_ip (ports 9100 / 9965).
  - node_nf_conntrack_stat_*, hubble_policy_verdict_total and coredns_forward_*
    are NOT exported; conntrack insert failures come from
    hubble_drop_total{reason="CT_MAP_INSERTION_FAILED"} instead.
  - ebpf histograms are in microseconds: divide quantiles by 1e6.
"""

import json
import pathlib

OUT_DIR = pathlib.Path(__file__).resolve().parents[2] / "deploy/helm/networkdoctor/dashboards"

DS = {"type": "prometheus", "uid": "${datasource}"}

# Join node-exporter / Hubble series (instance=<ip>:<port>) to a node name,
# same pattern as the scenario rules. The $node matcher rides on
# kube_node_info, which is the only series here that has a node label.
J9100 = ('* on (instance) group_left (node) max by (instance, node) '
         '(label_replace(kube_node_info{node=~"$node"}, "instance", "$1:9100", "internal_ip", "(.*)"))')
J9965 = ('* on (instance) group_left (node) max by (instance, node) '
         '(label_replace(kube_node_info{node=~"$node"}, "instance", "$1:9965", "internal_ip", "(.*)"))')

NIC = 'device!~"lo|veth.*|lxc.*|cilium.*|tailscale.*|docker.*"'


def panel(title, targets, unit="short", thresholds=(), description="", max_val=None):
    """A timeseries panel. targets: list of (expr, legendFormat)."""
    steps = [{"color": "green", "value": None}]
    for i, t in enumerate(thresholds):
        steps.append({"color": "#EAB839" if (i == 0 and len(thresholds) > 1) else "red", "value": t})
    p = {
        "type": "timeseries",
        "title": title,
        "description": description,
        "datasource": DS,
        "fieldConfig": {
            "defaults": {
                "unit": unit,
                "min": 0,
                "custom": {
                    "drawStyle": "line",
                    "lineWidth": 1,
                    "fillOpacity": 8,
                    "showPoints": "never",
                    "spanNulls": True,
                    "thresholdsStyle": {"mode": "line" if thresholds else "off"},
                },
                "thresholds": {"mode": "absolute", "steps": steps},
            },
            "overrides": [],
        },
        "options": {
            "legend": {"displayMode": "list", "placement": "bottom", "calcs": ["max", "lastNotNull"]},
            "tooltip": {"mode": "multi", "sort": "desc"},
        },
        "targets": [
            {"expr": expr, "legendFormat": legend, "refId": chr(65 + i), "datasource": DS}
            for i, (expr, legend) in enumerate(targets)
        ],
    }
    if max_val is not None:
        p["fieldConfig"]["defaults"]["max"] = max_val
    return p


def variable_node():
    return {
        "name": "node",
        "label": "node",
        "type": "query",
        "datasource": DS,
        "query": {"query": "label_values(ebpf_tcp_retransmits_total, node)", "refId": "var-node"},
        "refresh": 2,
        "multi": True,
        "includeAll": True,
        "allValue": ".*",
        "current": {"selected": True, "text": ["All"], "value": ["$__all"]},
        "sort": 1,
    }


def variable_service():
    return {
        "name": "service",
        "label": "service",
        "type": "query",
        "datasource": DS,
        "query": {"query": "label_values(http_requests_total, service)", "refId": "var-service"},
        "refresh": 2,
        "multi": True,
        "includeAll": True,
        "allValue": ".*",
        "current": {"selected": True, "text": ["All"], "value": ["$__all"]},
        "sort": 1,
    }


def dashboard(uid, title, rows, tags, annotations=(), description=""):
    """rows: list of (row_title, [panels]). Lays out two panels per line."""
    panels, y, pid = [], 0, 1
    for row_title, row_panels in rows:
        panels.append({
            "type": "row", "title": row_title, "collapsed": False,
            "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "id": pid, "panels": [],
        })
        pid += 1
        y += 1
        # Pack panels left to right, wrapping at 24 columns. Panels may carry
        # _w/_h hints (e.g. the gauge strip); default panels are 12x8, and a
        # trailing odd default panel stretches to the full width.
        x, line_h = 0, 0
        for i, p in enumerate(row_panels):
            p = dict(p)
            w = p.pop("_w", 12)
            h = p.pop("_h", 8)
            if w == 12 and i == len(row_panels) - 1 and x == 0:
                w = 24
            if x + w > 24:
                y += line_h
                x, line_h = 0, 0
            p["gridPos"] = {"h": h, "w": w, "x": x, "y": y}
            p["id"] = pid
            pid += 1
            panels.append(p)
            x += w
            line_h = max(line_h, h)
            if x >= 24:
                y += line_h
                x, line_h = 0, 0
        if x > 0:
            y += line_h

    anno_list = [{
        "builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"},
        "enable": True, "hide": True, "iconColor": "rgba(0, 211, 255, 1)",
        "name": "Annotations & Alerts", "type": "dashboard",
    }]
    for name, expr in annotations:
        anno_list.append({
            "datasource": DS, "enable": True, "expr": expr, "name": name,
            "iconColor": "rgba(255, 96, 96, 1)", "step": "30s",
            "titleFormat": "{{alertname}}", "tagKeys": "rule_id,node,service",
        })

    return {
        "uid": uid,
        "title": title,
        "description": description,
        "tags": tags,
        "timezone": "",
        "schemaVersion": 39,
        "version": 1,
        "editable": True,
        "graphTooltip": 1,
        "refresh": "30s",
        "time": {"from": "now-1h", "to": "now"},
        "links": [{
            "type": "dashboards", "tags": ["networkdoctor"], "asDropdown": True,
            "title": "NetworkDoctor", "includeVars": True, "keepTime": True,
        }],
        "templating": {"list": [
            {"name": "datasource", "label": "datasource", "type": "datasource",
             "query": "prometheus", "refresh": 1, "current": {}},
            variable_node(),
            variable_service(),
        ]},
        "annotations": {"list": anno_list},
        "panels": panels,
    }


def rule_annotation(rule_id):
    return [(f"{rule_id} firing",
             f'ALERTS{{source="networkdoctor", rule_id="{rule_id}", alertstate="firing"}}')]


# --------------------------------------------------------------------------
# Shared panel builders (same queries as the rules, so thresholds match)
# --------------------------------------------------------------------------

def p_app_p99(thresholds=(0.5,)):
    return panel(
        "App p99 latency by service/node",
        [('histogram_quantile(0.99, sum by (le, namespace, service, node) '
          '(rate(http_request_duration_seconds_bucket{node=~"$node", service=~"$service"}[5m])))',
          "{{namespace}}/{{service}} @ {{node}}")],
        unit="s", thresholds=thresholds,
        description="Rule 1 trigger: > 500ms p99 for 3m (with retransmits on the same node).")


def p_5xx_ratio(thresholds=(0.05,)):
    return panel(
        "HTTP 5xx ratio by service",
        [('sum by (namespace, service) (rate(http_requests_total{status=~"5..", service=~"$service"}[5m])) '
          '/ clamp_min(sum by (namespace, service) (rate(http_requests_total{service=~"$service"}[5m])), 0.001)',
          "{{namespace}}/{{service}}")],
        unit="percentunit", thresholds=thresholds, max_val=1,
        description="Rule 7 trigger: > 5% 5xx for 3m while the network is quiet.")


def p_req_rate():
    return panel(
        "HTTP request rate by service/status",
        [('sum by (namespace, service, status) (rate(http_requests_total{service=~"$service"}[5m]))',
          "{{namespace}}/{{service}} {{status}}")],
        unit="reqps")


def p_retransmits(thresholds=(0.2,)):
    return panel(
        "TCP retransmits by node",
        [('sum by (node) (rate(ebpf_tcp_retransmits_total{node=~"$node"}[5m]))', "{{node}}")],
        unit="ops", thresholds=thresholds,
        description="Rule 1 trigger / Rule 7 exclusion threshold: 0.2/s.")


def p_srtt():
    return panel(
        "TCP p99 sRTT by node",
        [('histogram_quantile(0.99, sum by (le, node) '
          '(rate(ebpf_tcp_srtt_microseconds_bucket{node=~"$node"}[5m]))) / 1e6', "{{node}}")],
        unit="s", description="eBPF smoothed RTT (µs histogram converted to seconds).")


def p_cwnd():
    return panel(
        "TCP mean cwnd by node",
        [('sum by (node) (rate(ebpf_tcp_cwnd_sum{node=~"$node"}[5m])) '
          '/ clamp_min(sum by (node) (rate(ebpf_tcp_cwnd_count{node=~"$node"}[5m])), 0.001)',
          "{{node}}")],
        unit="short", description="Falling or persistently low cwnd during rising RTT supports congestion.")


def p_connect_fail_ratio(thresholds=(0.1,)):
    return panel(
        "TCP connect failure ratio by node",
        [('sum by (node) (rate(ebpf_tcp_connect_failed_total{node=~"$node"}[5m])) '
          '/ clamp_min(sum by (node) (rate(ebpf_tcp_connect_attempts_total{node=~"$node"}[5m])), 0.001)',
          "{{node}}")],
        unit="percentunit", thresholds=thresholds, max_val=1,
        description="Rule 6 trigger: > 10% on exactly one node for 2m. Keep node=All to see the contrast.")


def p_conn_gauges():
    return panel(
        "TCP connections (active / TIME_WAIT) by node",
        [('sum by (node) (ebpf_tcp_connections_active{node=~"$node"})', "{{node}} active"),
         ('sum by (node) (ebpf_tcp_connections_time_wait{node=~"$node"})', "{{node}} time_wait")],
        unit="short")


def p_conn_churn():
    return panel(
        "TCP connect attempts / short-lived conns by node",
        [('sum by (node) (rate(ebpf_tcp_connect_attempts_total{node=~"$node"}[5m]))', "{{node}} attempts"),
         ('sum by (node) (rate(ebpf_tcp_connections_short_lived_total{node=~"$node"}[5m]))', "{{node}} short-lived")],
        unit="ops", description="Connection churn feeds the conntrack table.")


def p_softnet(thresholds=(1,)):
    return panel(
        "Softnet dropped / squeezed by node",
        [(f'sum by (node) (rate(node_softnet_dropped_total[5m]) {J9100})', "{{node}} dropped"),
         (f'sum by (node) (rate(node_softnet_times_squeezed_total[5m]) {J9100})', "{{node}} squeezed")],
        unit="ops", thresholds=thresholds,
        description="Rule 2 trigger: softnet drops > 1/s. Squeezed = budget exhausted, pressure precursor.")


def p_nic_drops(thresholds=(10,)):
    return panel(
        "Physical NIC drops by node",
        [(f'sum by (node) (rate(node_network_receive_drop_total{{{NIC}}}[5m]) {J9100})', "{{node}} rx"),
         (f'sum by (node) (rate(node_network_transmit_drop_total{{{NIC}}}[5m]) {J9100})', "{{node}} tx")],
        unit="ops", thresholds=thresholds,
        description="Rule 2 trigger: rx drops > 10/s on physical interfaces (veth/cilium/etc. excluded).")


def p_nic_errs():
    return panel(
        "Physical NIC errors by node",
        [(f'sum by (node) (rate(node_network_receive_errs_total{{{NIC}}}[5m]) {J9100})', "{{node}} rx"),
         (f'sum by (node) (rate(node_network_transmit_errs_total{{{NIC}}}[5m]) {J9100})', "{{node}} tx")],
        unit="ops")


def p_nic_throughput():
    return panel(
        "Physical NIC throughput by node",
        [(f'sum by (node) (rate(node_network_receive_bytes_total{{{NIC}}}[5m]) {J9100})', "{{node}} rx"),
         (f'sum by (node) (rate(node_network_transmit_bytes_total{{{NIC}}}[5m]) {J9100})', "{{node}} tx")],
        unit="Bps")


def p_cpu_softirq():
    return panel(
        "CPU softirq / system time by node",
        [(f'sum by (node) (rate(node_cpu_seconds_total{{mode="softirq"}}[5m]) {J9100})', "{{node}} softirq"),
         (f'sum by (node) (rate(node_cpu_seconds_total{{mode="system"}}[5m]) {J9100})', "{{node}} system")],
        unit="short", description="Cores spent in softirq; sustained growth means packet processing pressure.")


def p_runqlat():
    return panel(
        "Run queue p99 latency by node",
        [('histogram_quantile(0.99, sum by (le, node) '
          '(rate(ebpf_runqlat_bucket{node=~"$node"}[5m]))) / 1e6', "{{node}}")],
        unit="s",
        description="Scheduling delay. If this rises while TCP RTT/retransmits stay flat, the cause is CPU, not network.")


def p_conntrack_ratio(thresholds=(0.5, 0.8)):
    return panel(
        "Conntrack table usage by node",
        [(f'(node_nf_conntrack_entries / node_nf_conntrack_entries_limit) {J9100}', "{{node}}")],
        unit="percentunit", thresholds=thresholds, max_val=1,
        description="Rule 3 trigger: > 80%. Rule 5 correlates slow DNS with > 50%.")


def p_conntrack_abs():
    return panel(
        "Conntrack entries vs limit by node",
        [(f'node_nf_conntrack_entries {J9100}', "{{node}} entries"),
         (f'node_nf_conntrack_entries_limit {J9100}', "{{node}} limit")],
        unit="short")


def p_ct_insert_failed():
    return panel(
        "Cilium CT map insertion failures by node",
        [(f'sum by (node) (rate(hubble_drop_total{{reason="CT_MAP_INSERTION_FAILED"}}[5m]) {J9965})',
          "{{node}}")],
        unit="ops",
        description="Rule 3 trigger: any sustained value. node_nf_conntrack_stat_* is not exported on the lab cluster; this is the drop-side evidence.")


def p_tcp_opens():
    return panel(
        "Kernel TCP opens by node",
        [(f'sum by (node) (rate(node_netstat_Tcp_ActiveOpens[5m]) {J9100})', "{{node}} active"),
         (f'sum by (node) (rate(node_netstat_Tcp_PassiveOpens[5m]) {J9100})', "{{node}} passive")],
        unit="ops")


def p_tcp_fails():
    # node_netstat_Tcp_AttemptFails / Tcp_EstabResets are not in
    # node-exporter's default netstat whitelist, so they don't exist on the
    # lab cluster; eBPF connect failures + outgoing RSTs cover the same signal.
    return panel(
        "TCP connect failures / RSTs by node",
        [('sum by (node) (rate(ebpf_tcp_connect_failed_total{node=~"$node"}[5m]))', "{{node}} connect failed"),
         (f'sum by (node) (rate(node_netstat_Tcp_OutRsts[5m]) {J9100})', "{{node}} RST sent")],
        unit="ops")


def p_sockstat():
    return panel(
        "Sockets (alloc / TIME_WAIT) by node",
        [(f'sum by (node) (node_sockstat_TCP_alloc {J9100})', "{{node}} TCP alloc"),
         (f'sum by (node) (node_sockstat_TCP_tw {J9100})', "{{node}} TCP tw"),
         (f'sum by (node) (node_sockstat_UDP_inuse {J9100})', "{{node}} UDP inuse")],
        unit="short")


def p_coredns_p99(thresholds=(0.25,)):
    return panel(
        "CoreDNS p99 latency",
        [('histogram_quantile(0.99, sum by (le, namespace, service) '
          '(rate(coredns_dns_request_duration_seconds_bucket[5m])))', "{{namespace}}/{{service}}")],
        unit="s", thresholds=thresholds,
        description="Rule 4 trigger: > 250ms for 5m.")


def p_servfail_ratio(thresholds=(0.05,)):
    return panel(
        "CoreDNS SERVFAIL ratio",
        [('sum by (namespace, service) (rate(coredns_dns_responses_total{rcode="SERVFAIL"}[5m])) '
          '/ clamp_min(sum by (namespace, service) (rate(coredns_dns_responses_total[5m])), 0.001)',
          "{{namespace}}/{{service}}")],
        unit="percentunit", thresholds=thresholds, max_val=1,
        description="Rule 4 trigger: > 5% for 5m. NXDOMAIN is deliberately excluded (search domains make it routine).")


def p_coredns_rcodes():
    return panel(
        "CoreDNS responses by rcode",
        [('sum by (rcode) (rate(coredns_dns_responses_total[5m]))', "{{rcode}}")],
        unit="reqps")


def p_coredns_per_instance():
    return panel(
        "CoreDNS p99 per instance",
        [('histogram_quantile(0.99, sum by (le, instance) '
          '(rate(coredns_dns_request_duration_seconds_bucket[5m])))', "{{instance}}")],
        unit="s",
        description="One slow instance vs peers = pod/node-local problem; all slow together = shared/upstream cause.")


def p_coredns_pod_health():
    return panel(
        "CoreDNS pod CPU / restarts",
        [('sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="kube-system", pod=~"coredns.*"}[5m]))',
          "{{pod}} cpu"),
         ('sum by (pod) (kube_pod_container_status_restarts_total{namespace="kube-system", pod=~"coredns.*"})',
          "{{pod}} restarts")],
        unit="short")


def p_dns_slow(thresholds=(0.1,)):
    return panel(
        "Slow DNS responses by node (eBPF)",
        [('sum by (node) (rate(ebpf_dns_slow_total{node=~"$node"}[5m]))', "{{node}}")],
        unit="ops", thresholds=thresholds,
        description="Rule 5 trigger: > 0.1/s. Responses slower than the agent threshold (default 50ms); closest proxy for timeouts.")


def p_dns_client_p99():
    return panel(
        "Client DNS p99 latency by node (eBPF)",
        [('histogram_quantile(0.99, sum by (le, node) '
          '(rate(ebpf_dns_query_latency_bucket{node=~"$node"}[5m]))) / 1e6', "{{node}}")],
        unit="s", description="What pods on each node actually experience, regardless of which CoreDNS answered.")


def p_dns_rcodes_client():
    return panel(
        "Client DNS responses by rcode (eBPF)",
        [('sum by (rcode) (rate(ebpf_dns_rcode_total{node=~"$node"}[5m]))', "{{rcode}}")],
        unit="reqps")


def p_policy_denied():
    return panel(
        "Policy-denied drops by node (Hubble)",
        [(f'sum by (node) (rate(hubble_drop_total{{reason=~"POLICY_DENIED|POLICY_DENY"}}[5m]) {J9965})',
          "{{node}}")],
        unit="ops", description="Rule 8 trigger: any sustained value for 2m.")


def p_hubble_drops_by_reason():
    return panel(
        "Hubble drops by reason",
        [('sum by (reason) (rate(hubble_drop_total[5m]))', "{{reason}}")],
        unit="ops",
        description="hubble_* has no node label; this is the cluster-wide breakdown.")


def p_node_pressure():
    return panel(
        "Node pressure conditions",
        [('sum by (node, condition) (kube_node_status_condition{status="true", '
          'condition=~"MemoryPressure|DiskPressure|PIDPressure|NetworkUnavailable"})',
          "{{node}} {{condition}}")],
        unit="short", description="Non-zero means the kubelet reports that pressure condition.")


def p_node_saturation():
    return panel(
        "Node load / memory by node",
        [(f'sum by (node) (node_load1 {J9100})', "{{node}} load1"),
         (f'(node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) {J9100}', "{{node}} mem avail ratio")],
        unit="short")


def p_disk_io():
    return panel(
        "Disk IO time by node",
        [(f'sum by (node) (rate(node_disk_io_time_seconds_total[5m]) {J9100})', "{{node}}")],
        unit="percentunit")


def p_alerts_firing():
    return panel(
        "Firing NetworkDoctor alerts",
        [('count by (rule_id, alertname) (ALERTS{source="networkdoctor", alertstate="firing"})',
          "{{rule_id}} {{alertname}}")],
        unit="short",
        description="Every scenario rule currently firing. Jump to the matching nd-rule-N incident dashboard.")


def p_rule_proximity():
    """One horizontal bar per rule: current trigger value / rule threshold.

    1.0 (100%) = the trigger condition is met right now (the `for:` clock is
    running). AND-rules (1, 5) take the LOWER of their two ratios via
    clamp_max(vec, scalar) — both signals must exceed. OR-rules (2, 3, 4) take
    the HIGHER via clamp_min. Rule 7 is gated to 0 while the network is noisy
    (its `unless` clause). Rules 3/8 fire on "any drops", rendered as
    0.01 drops/s = 100%. `or vector(0)` keeps bars at 0 when a series does
    not exist yet (e.g. no Hubble drop seen so far).
    """
    lat = ('max(histogram_quantile(0.99, sum by (le, namespace, service, node) '
           '(rate(http_request_duration_seconds_bucket[5m]))))')
    retrans = 'max(sum by (node) (rate(ebpf_tcp_retransmits_total[5m])))'
    ct_ratio = f'max((node_nf_conntrack_entries / node_nf_conntrack_entries_limit) {J9100})'
    ct_fail = (f'max(sum by (node) (rate(hubble_drop_total{{reason="CT_MAP_INSERTION_FAILED"}}[5m]) {J9965}))')
    servfail = ('max(sum by (namespace, service) (rate(coredns_dns_responses_total{rcode="SERVFAIL"}[5m])) '
                '/ clamp_min(sum by (namespace, service) (rate(coredns_dns_responses_total[5m])), 0.001))')
    coredns = ('max(histogram_quantile(0.99, sum by (le, namespace, service) '
               '(rate(coredns_dns_request_duration_seconds_bucket[5m]))))')
    fivexx = ('max(sum by (namespace, service) (rate(http_requests_total{status=~"5.."}[5m])) '
              '/ clamp_min(sum by (namespace, service) (rate(http_requests_total[5m])), 0.001))')
    connfail = ('max(sum by (node) (rate(ebpf_tcp_connect_failed_total[5m])) '
                '/ clamp_min(sum by (node) (rate(ebpf_tcp_connect_attempts_total[5m])), 0.001))')
    dns_slow = 'max(sum by (node) (rate(ebpf_dns_slow_total[5m])))'
    denied = (f'max(sum by (node) (rate(hubble_drop_total{{reason=~"POLICY_DENIED|POLICY_DENY"}}[5m]) {J9965}))')

    targets = [
        (f'clamp_max({lat} / 0.5, scalar({retrans} / 0.2 or vector(0))) or vector(0)',
         "Rule 1 · Network congestion  (p99 AND retransmits — lower of the two)"),
        (f'clamp_min(max(sum by (node) (rate(node_softnet_dropped_total[5m]) {J9100})) / 1, '
         f'scalar(max(sum by (node) (rate(node_network_receive_drop_total{{{NIC}}}[5m]) {J9100})) / 10 '
         'or vector(0))) or vector(0)',
         "Rule 2 · Kernel bottleneck  (softnet OR NIC drops — higher of the two)"),
        (f'clamp_min({ct_ratio} / 0.8, scalar({ct_fail} / 0.01 or vector(0))) or vector(0)',
         "Rule 3 · Conntrack exhaustion  (usage/80% OR CT insert failures)"),
        (f'clamp_min({coredns} / 0.25, scalar({servfail} / 0.05 or vector(0))) or vector(0)',
         "Rule 4 · CoreDNS degradation  (p99/250ms OR SERVFAIL/5%)"),
        (f'clamp_max({dns_slow} / 0.1, scalar({ct_ratio} / 0.5 or vector(0))) or vector(0)',
         "Rule 5 · DNS × conntrack  (slow DNS AND conntrack/50% — lower of the two)"),
        (f'{connfail} / 0.1 or vector(0)',
         "Rule 6 · Node-localized failure  (worst node connect-fail ratio / 10%)"),
        (f'({fivexx} / 0.05) * scalar({retrans} <= bool 0.2) or vector(0)',
         "Rule 7 · App-induced  (5xx/5%, zeroed while the network is noisy)"),
        (f'{denied} / 0.01 or vector(0)',
         "Rule 8 · NetworkPolicy denied  (POLICY_DENIED drops, 0.01/s = 100%)"),
    ]
    return {
        "type": "bargauge",
        "title": "Rule firing proximity — 100% = trigger condition met, for: timer starts",
        "description": "Each bar replays its rule's trigger query divided by the rule threshold, "
                       "so all eight rules land on one scale: how close is this rule to firing. "
                       "Yellow from 70%, red at 100%. Actually-firing alerts (trigger held for the "
                       "rule's `for:` duration) appear in the panel below. Panel links (top-left "
                       "corner menu) jump to each rule's incident dashboard.",
        "datasource": DS,
        "fieldConfig": {
            "defaults": {
                "unit": "percentunit",
                "min": 0,
                "max": 1.2,
                "thresholds": {"mode": "absolute", "steps": [
                    {"color": "green", "value": None},
                    {"color": "#EAB839", "value": 0.7},
                    {"color": "red", "value": 1},
                ]},
            },
            "overrides": [],
        },
        "options": {
            "orientation": "horizontal",
            "displayMode": "basic",
            "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "showUnfilled": True,
            "valueMode": "color",
        },
        "targets": [
            {"expr": expr, "legendFormat": legend, "refId": chr(65 + i), "datasource": DS}
            for i, (expr, legend) in enumerate(targets)
        ],
        "links": [{"title": f"Rule {n} incident dashboard", "url": f"/d/nd-rule-{n}"}
                  for n in range(1, 9)],
        "_h": 9,
    }


# --------------------------------------------------------------------------
# Dashboards
# --------------------------------------------------------------------------

def overview():
    rows = [
        ("NetworkDoctor rules", [p_rule_proximity(), p_alerts_firing()]),
        ("L7 — Application (Rule 1·7)", [p_app_p99(), p_5xx_ratio(), p_req_rate()]),
        ("DNS (Rule 4·5)", [p_coredns_p99(), p_servfail_ratio(), p_dns_slow(), p_dns_client_p99()]),
        ("TCP — eBPF (Rule 1·6)", [p_retransmits(), p_connect_fail_ratio(), p_srtt(), p_conn_gauges()]),
        ("Kernel / NIC (Rule 2)", [p_softnet(), p_nic_drops(), p_nic_throughput(), p_runqlat()]),
        ("Conntrack (Rule 3·5)", [p_conntrack_ratio(), p_ct_insert_failed()]),
        ("CNI — Cilium (Rule 8)", [p_policy_denied(), p_hubble_drops_by_reason()]),
    ]
    return dashboard(
        "nd-overview", "NetworkDoctor / Overview", rows,
        ["networkdoctor", "overview"],
        annotations=[("NetworkDoctor firing",
                      'ALERTS{source="networkdoctor", alertstate="firing"}')],
        description="Always-on layer map, L7 down to the CNI. Every panel reuses the exact "
                    "query and threshold of the scenario rule that watches that layer, so a "
                    "line crossing a threshold here is the rule about to fire.")


def rule1():
    rows = [
        ("Trigger — why the rule fired", [p_app_p99(), p_retransmits()]),
        ("Supporting — is it really congestion", [p_srtt(), p_cwnd(), p_nic_drops(thresholds=()), p_nic_throughput()]),
        ("Exclusion — other causes", [p_runqlat(), p_coredns_p99(), p_conntrack_ratio(), p_policy_denied()]),
    ]
    return dashboard(
        "nd-rule-1", "NetworkDoctor / Rule 1 — Network Congestion", rows,
        ["networkdoctor", "incident", "rule-1"], annotations=rule_annotation("rule-1"),
        description="p99 latency and TCP retransmits up on the same node. Supporting: RTT up, "
                    "cwnd down, NIC pressure. Excluded: CPU scheduling delay, DNS, conntrack, policy.")


def rule2():
    rows = [
        ("Trigger", [p_softnet(), p_nic_drops()]),
        ("Supporting", [p_cpu_softirq(), p_nic_throughput(), p_nic_errs(), p_runqlat()]),
        ("Exclusion", [p_conntrack_ratio(), p_ct_insert_failed(), p_policy_denied(), p_coredns_p99()]),
    ]
    return dashboard(
        "nd-rule-2", "NetworkDoctor / Rule 2 — Kernel Network Bottleneck", rows,
        ["networkdoctor", "incident", "rule-2"], annotations=rule_annotation("rule-2"),
        description="Softnet or physical NIC drops. Supporting: softirq CPU, throughput vs errors, "
                    "run queue. Excluded: conntrack, policy denial, DNS.")


def rule3():
    rows = [
        ("Trigger", [p_conntrack_ratio(), p_ct_insert_failed()]),
        ("Supporting", [p_conntrack_abs(), p_tcp_opens(), p_tcp_fails(), p_sockstat(),
                        p_conn_churn(), p_conn_gauges()]),
        ("Exclusion", [p_coredns_p99(), p_softnet(thresholds=()), p_policy_denied()]),
    ]
    return dashboard(
        "nd-rule-3", "NetworkDoctor / Rule 3 — Conntrack Exhaustion", rows,
        ["networkdoctor", "incident", "rule-3"], annotations=rule_annotation("rule-3"),
        description="Conntrack table near full or CT map insert failures. Supporting: connection "
                    "churn (opens, short-lived, TIME_WAIT). Excluded: DNS-first, kernel drops, policy.")


def rule4():
    rows = [
        ("Trigger", [p_coredns_p99(), p_servfail_ratio()]),
        ("Supporting", [p_coredns_rcodes(), p_dns_client_p99(), p_dns_slow(thresholds=()),
                        p_coredns_pod_health()]),
        ("Exclusion", [p_coredns_per_instance(), p_conntrack_ratio(), p_softnet(thresholds=()),
                       p_policy_denied()]),
    ]
    return dashboard(
        "nd-rule-4", "NetworkDoctor / Rule 4 — CoreDNS Degradation", rows,
        ["networkdoctor", "incident", "rule-4"], annotations=rule_annotation("rule-4"),
        description="CoreDNS slow or SERVFAIL (NXDOMAIN excluded: search domains make it routine). "
                    "Supporting: client-side eBPF DNS view, CoreDNS pod health. Exclusion: one "
                    "instance vs peers, conntrack, kernel, policy.")


def rule5():
    rows = [
        ("Trigger", [p_dns_slow(), p_conntrack_ratio()]),
        ("Supporting", [
            panel("Onset timing — slow DNS vs conntrack (1m windows)",
                  [('sum by (node) (rate(ebpf_dns_slow_total{node=~"$node"}[1m]))', "{{node}} dns slow"),
                   (f'(node_nf_conntrack_entries / node_nf_conntrack_entries_limit) {J9100}',
                    "{{node}} conntrack ratio")],
                  unit="short",
                  description="Correlation check: conntrack pressure should rise before or with the "
                              "DNS slowdown. 1m windows keep the onsets sharp."),
            p_dns_client_p99(), p_ct_insert_failed(), p_sockstat(), p_conn_churn()]),
        ("Exclusion", [p_coredns_p99(), p_coredns_pod_health(), p_policy_denied()]),
    ]
    return dashboard(
        "nd-rule-5", "NetworkDoctor / Rule 5 — DNS × Conntrack Correlation", rows,
        ["networkdoctor", "incident", "rule-5"], annotations=rule_annotation("rule-5"),
        description="Slow DNS together with conntrack pressure on the same node. Supporting: onset "
                    "timing, UDP sockets, churn. Excluded: CoreDNS-server-side cause, policy.")


def rule6():
    rows = [
        ("Trigger", [p_connect_fail_ratio()]),
        ("Supporting", [p_retransmits(thresholds=()), p_srtt(), p_nic_drops(thresholds=()), p_nic_errs(),
                        p_node_pressure(), p_node_saturation(), p_runqlat(), p_disk_io()]),
        ("Exclusion", [p_5xx_ratio(thresholds=()), p_coredns_p99(), p_conntrack_ratio(), p_policy_denied()]),
    ]
    return dashboard(
        "nd-rule-6", "NetworkDoctor / Rule 6 — Node-Localized Failure", rows,
        ["networkdoctor", "incident", "rule-6"], annotations=rule_annotation("rule-6"),
        description="Connect failures concentrated on exactly one node — keep node=All so the "
                    "contrast against healthy peers is visible. Supporting: everything node-local. "
                    "Excluded: single-workload (app) causes, DNS, conntrack, policy.")


def rule7():
    rows = [
        ("Trigger", [p_5xx_ratio(), p_retransmits()]),
        ("Supporting", [p_req_rate(), p_app_p99(thresholds=()), p_conn_churn(), p_tcp_opens(),
                        p_conn_gauges(), p_tcp_fails()]),
        ("Exclusion", [p_softnet(thresholds=()), p_nic_drops(), p_conntrack_ratio(),
                       p_coredns_p99(), p_policy_denied()]),
    ]
    return dashboard(
        "nd-rule-7", "NetworkDoctor / Rule 7 — Application-Induced Bottleneck", rows,
        ["networkdoctor", "incident", "rule-7"], annotations=rule_annotation("rule-7"),
        description="5xx while the network is quiet (retransmits must stay under 0.2/s — that panel "
                    "staying flat IS the evidence). Supporting: traffic surge / retry churn. "
                    "Exclusion row should all be flat; anything elevated reclassifies the incident.")


def rule8():
    rows = [
        ("Trigger", [p_policy_denied()]),
        ("Supporting", [p_hubble_drops_by_reason(), p_connect_fail_ratio(thresholds=()),
                        p_5xx_ratio(thresholds=()), p_app_p99(thresholds=())]),
        ("Exclusion", [p_conntrack_ratio(), p_ct_insert_failed(), p_softnet(thresholds=()),
                       p_coredns_p99()]),
    ]
    return dashboard(
        "nd-rule-8", "NetworkDoctor / Rule 8 — NetworkPolicy Misconfiguration", rows,
        ["networkdoctor", "incident", "rule-8"], annotations=rule_annotation("rule-8"),
        description="Cilium dropping traffic by policy. Supporting: drop-reason breakdown and the "
                    "app-side impact (connect failures, 5xx). Excluded: conntrack (CT drops have a "
                    "different reason), kernel, DNS. Flow-level attribution needs Hubble CLI, not metrics.")


def main():
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    dashboards = {
        "nd-overview.json": overview(),
        "nd-rule-1.json": rule1(),
        "nd-rule-2.json": rule2(),
        "nd-rule-3.json": rule3(),
        "nd-rule-4.json": rule4(),
        "nd-rule-5.json": rule5(),
        "nd-rule-6.json": rule6(),
        "nd-rule-7.json": rule7(),
        "nd-rule-8.json": rule8(),
    }
    for name, dash in dashboards.items():
        path = OUT_DIR / name
        path.write_text(json.dumps(dash, indent=1, ensure_ascii=False) + "\n")
        print(f"wrote {path.relative_to(OUT_DIR.parents[3])}")


if __name__ == "__main__":
    main()
