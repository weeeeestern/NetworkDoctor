---
name: conntrack-exhaustion
description: Investigate Network Doctor Rule 3 alerts for Linux conntrack table exhaustion, insert failures, connection churn, TIME_WAIT growth, TCP connect failures, and related Kubernetes service disruption.
last_updated: 2026-09-30
---

## Goal

Determine whether a Rule 3 alert is caused by conntrack table exhaustion or connection churn, and distinguish it from DNS-only failure, NetworkPolicy denial, node CPU contention, or application connection leaks.

## Trigger Criteria

Use this skill only when:

- `rule_id`, `alertname`, or annotation indicates Network Doctor Rule 3.
- The alert mentions conntrack usage, conntrack limit, insert failure, connection churn, TCP connect failure, or sudden connection reset/timeout patterns.

Do not use this skill for pure CoreDNS latency unless conntrack pressure is part of the alert; use Rule 5 for explicit DNS plus conntrack correlation.

## Investigation Scope

- Set `alert_time` from `startsAt`; if missing, use current time and state that assumption.
- Primary window: `alert_time - 15m` to `alert_time + 15m`.
- Baseline window: previous 1h for the affected node/service.
- Scope by affected node first. If no node is provided, infer top nodes by conntrack usage ratio and insert failures.
- Compare affected node against peer nodes and cluster median.
- Inspect at most the top five namespaces/workloads by connection churn.

## Data Sources and Metrics

Prometheus metrics:

- Conntrack occupancy: `node_nf_conntrack_entries`, `node_nf_conntrack_entries_limit`.
- Conntrack failures: `node_nf_conntrack_stat_insert_failed`, `node_nf_conntrack_stat_drop`, `node_nf_conntrack_stat_early_drop`, `node_nf_conntrack_stat_error`, `node_nf_conntrack_stat_search_restart`.
- TCP connection churn: `node_netstat_Tcp_ActiveOpens`, `node_netstat_Tcp_PassiveOpens`, `node_netstat_Tcp_AttemptFails`, `node_netstat_Tcp_EstabResets`, `node_netstat_Tcp_CurrEstab`.
- Socket state: `node_sockstat_TCP_alloc`, `node_sockstat_TCP_inuse`, `node_sockstat_TCP_tw`, `node_sockstat_sockets_used`.
- Network Doctor eBPF: `ebpf_tcp_connect_attempts_total`, `ebpf_tcp_connect_failed_total`, `ebpf_tcp_retransmits_total`, `ebpf_tcp_srtt_microseconds`.
- Optional app/client signals: `http_requests_total`, `http_client_requests_total`, retry counters such as `application_retries_total` or `client_retries_total` if present.
- DNS impact if present: `ebpf_dns_query_latency_bucket`, `coredns_dns_request_duration_seconds_bucket`, `coredns_dns_responses_total`.

Holmes toolsets:

- Prometheus range/instant queries and metadata discovery.
- Kubernetes read queries for pods, services, endpoints, events, and logs of implicated clients.
- Hubble flow summaries, drops, service flows, and port flows if available.

## NetworkDoctor eBPF metric reference (verified)

The agent exports these names (checked in Prometheus on the on-prem lab, 2026-09-28). Use them as written; do not guess suffixes.

- Counters: `ebpf_tcp_retransmits_total`, `ebpf_tcp_retransmit_bursts_total`, `ebpf_tcp_connect_attempts_total`, `ebpf_tcp_connect_failed_total`, `ebpf_tcp_connections_short_lived_total`, `ebpf_tcp_connections_closed_total`, `ebpf_dns_queries_total`, `ebpf_dns_responses_total`, `ebpf_dns_rcode_errors_total`, `ebpf_dns_rcode_total{rcode}`, `ebpf_dns_slow_total`, `ebpf_udp_packets_total`, `ebpf_udp_long_flows_total`.
- Gauges: `ebpf_tcp_connections_active`, `ebpf_tcp_connections_time_wait`.
- Histograms (`_bucket`/`_sum`/`_count`, `le` in powers of two): `ebpf_tcp_srtt_microseconds` (µs), `ebpf_tcp_cwnd` (segments), `ebpf_dns_query_latency` (µs), `ebpf_runqlat` (µs), `ebpf_udp_session_duration` (µs).
- Granularity: every `ebpf_*` series is **per node only** (labels `node`, `instance`, `pod` of the agent). There are no per-flow, per-pod or per-service labels such as `src_pod`/`dst_pod`; use Hubble or application metrics for flow-level attribution.
- There is no DNS timeout counter. `ebpf_dns_slow_total` counts responses slower than the agent threshold (default 50 ms) and is the closest proxy.
- Convert µs histograms before comparing to seconds: `histogram_quantile(0.99, sum by (le, node) (rate(ebpf_runqlat_bucket[5m]))) / 1e6`.

## Workflow

1. Normalize alert scope.
   - Extract affected node, namespace, workload, service, pod, destination, port, protocol, and alert threshold.
   - If node is missing, infer top nodes by conntrack ratio and insert failures.

2. Confirm conntrack pressure.
   - Query usage ratio over the primary window.
   - Query insert failures, drops, early drops, and errors.
   - Compare affected node with peers.

Example PromQL patterns:

```promql
node_nf_conntrack_entries{instance=~"$instance"}
/
clamp_min(node_nf_conntrack_entries_limit{instance=~"$instance"}, 1)

sum by (instance) (rate(node_nf_conntrack_stat_insert_failed{instance=~"$instance"}[5m]))

sum by (instance) (rate(node_nf_conntrack_stat_drop{instance=~"$instance"}[5m]))
```

3. Measure connection churn and socket state.
   - Query active/passive opens, attempt failures, resets, current established connections, TCP allocated, and TIME_WAIT.
   - Identify whether pressure comes from many short-lived connections, long-lived connection growth, or failed retries.

4. Find likely workload contributors.
   - Use Hubble flow summaries scoped to affected namespace/node/service.
   - Query per-workload request rate and retry counters if app metrics exist.
   - Inspect Kubernetes pod placement and recent rollout events for workloads on the affected node.

5. Check impact signals.
   - Query TCP connect errors, retransmissions, RTT, and application 5xx/timeout rates.
   - If DNS errors rise after conntrack failures, note possible Rule 5 correlation but finish Rule 3 evidence first.

6. Exclude alternatives.
   - NetworkPolicy: Hubble denied flows explain failed connections without conntrack pressure.
   - DNS-only: DNS latency/errors rise while conntrack usage and insert failures are normal.
   - Kernel CPU/NIC: SoftIRQ/softnet drops are first abnormal signals with normal conntrack ratio.
   - Application bug: one client opens excessive connections but conntrack limit is not close to pressure; report as app-induced churn rather than table exhaustion.

## Evidence and Exclusion Logic

Confirm conntrack exhaustion when:

- `node_nf_conntrack_entries / node_nf_conntrack_entries_limit` is high, typically above 0.85, and rising near `alert_time`.
- Insert failures, drops, early drops, or errors are non-zero and increase.
- TCP connect failures, resets, retransmissions, DNS failures, or application timeouts follow the conntrack pressure.

Classify connection churn when:

- Active opens and TIME_WAIT grow sharply.
- Conntrack occupancy rises without one long-lived connection owner.
- Workload request/retry metrics or Hubble flows show high short-lived traffic.

Exclude conntrack as primary cause when:

- Usage ratio remains low and insert failures are absent.
- Hubble shows policy denial for the affected flow.
- DNS/CoreDNS degradation precedes any conntrack symptom.

## Stop Conditions

Stop once:

- Conntrack exhaustion is confirmed with occupancy plus failure evidence.
- Connection churn is identified as the immediate cause without table exhaustion.
- Conntrack pressure is excluded and a stronger Rule 4, Rule 5, Rule 8, or Rule 7 path is evident.

Avoid broad log searches. Only fetch logs for specific implicated client/server pods and specific patterns such as `connect`, `timeout`, `connection reset`, or `too many open files`.

## Output Schema

Every NetworkDoctor skill returns the same top-level structure so the backend can parse any result the same way. Put scenario-specific findings under `scenario_details`, never as new top-level keys. Return it as a single fenced YAML block, after any prose summary.

```yaml
rule_id: "rule-3"
scenario: "conntrack-exhaustion"
investigation_status: "confirmed | excluded | inconclusive"
confidence: "high | medium | low"
time_window:
  alert_time: "<RFC3339; from startsAt, or now if missing (say so)>"
  start: "<RFC3339>"
  end: "<RFC3339>"
affected_resources:
  cluster: "<cluster or unknown>"
  namespaces: ["<namespace>"]
  services: ["<service>"]
  workloads: ["<workload>"]
  pods: ["<pod>"]
  nodes: ["<node>"]
root_cause: "<one or two sentences; the most likely cause, or why it is excluded>"
trigger_evidence:
  - source: "<metric query | tool name>"
    observation: "<what changed, with numbers and time>"
supporting_evidence:
  - source: "<metric query | tool name>"
    observation: "<independent signal that agrees>"
excluded_alternatives:
  - hypothesis: "<alternative cause, e.g. another NetworkDoctor rule>"
    reason: "<evidence that rules it out>"
recommended_actions:
  - "<read-only next check or operator action; never executed by Holmes>"
additional_checks:
  - "<data that was missing or should be collected next>"
scenario_details: {}   # optional; for this scenario: `affected_nodes`, `classification`, `top_suspected_sources`, `cross_node_comparison`
```

## Safety and Read-Only Constraints

- Do not change `nf_conntrack_max`, sysctls, kube-proxy, Cilium, or workload configs.
- Do not restart pods or nodes.
- Do not apply traffic shaping or policy changes.
- Do not fetch Secrets or credentials.
- Use only read-only metrics, Kubernetes queries, Hubble/Cilium observations, and scoped pod log reads.

