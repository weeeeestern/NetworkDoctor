---
name: node-localized-failure
description: Investigate Network Doctor Rule 6 alerts for a Kubernetes network failure localized to one node by comparing node CPU, iowait, SoftIRQ, NIC drops, conntrack, eBPF latency, pod placement, Cilium health, and peer nodes.
last_updated: 2026-09-30
---

## Goal

Determine whether an incident is localized to a specific Kubernetes node and identify the most likely node-level cause: CPU pressure, disk/iowait, SoftIRQ/NIC bottleneck, conntrack pressure, Cilium endpoint/node health, noisy workload, or hardware/host-level symptoms.

## Trigger Criteria

Use this skill only when:

- `rule_id`, `alertname`, or annotation indicates Network Doctor Rule 6.
- The alert says a specific node, worker, node pool, or host has degraded network behavior compared with other nodes.
- Multiple pods or services on the same node are impacted, or the alert includes node-level evidence.

Do not use this skill for a single service-only regression unless the same node-local pattern is evident.

## Investigation Scope

- Set `alert_time` from `startsAt`; if missing, use current time and state that assumption.
- Primary window: `alert_time - 15m` to `alert_time + 15m`.
- Baseline window: previous 1h for the affected node.
- Peer comparison: nodes with the same role, zone, instance type, kernel version, or node pool.
- Inspect pods scheduled on the affected node and a small peer sample.
- If no node is named, infer at most five candidate nodes from the most abnormal Network Doctor node metrics.

## Data Sources and Metrics

Prometheus metrics:

- Node identity: `kube_node_info`, `node_uname_info`, `kube_node_labels`.
- Node health: `kube_node_status_condition`, `kube_node_spec_unschedulable`.
- CPU: `node_cpu_seconds_total`, `node_load1`, `node_load5`, `ebpf_runqlat_bucket` or `ebpf_runqlat_bucket`.
- Memory: `node_memory_MemAvailable_bytes`, `node_memory_MemTotal_bytes`, `node_vmstat_pgmajfault`.
- Disk/iowait: `node_disk_io_time_seconds_total`, `node_disk_read_time_seconds_total`, `node_disk_write_time_seconds_total`, `node_filesystem_avail_bytes`.
- SoftIRQ/NIC: `node_softnet_dropped_total`, `node_softnet_times_squeezed_total`, `node_network_receive_drop_total`, `node_network_transmit_drop_total`, `node_network_receive_errs_total`, `node_network_transmit_errs_total`, `node_network_receive_bytes_total`, `node_network_transmit_bytes_total`.
- Conntrack: `node_nf_conntrack_entries`, `node_nf_conntrack_entries_limit`, `node_nf_conntrack_stat_insert_failed`.
- eBPF network: `ebpf_tcp_srtt_microseconds`, `ebpf_tcp_retransmits_total`, `ebpf_dns_slow_total`, `ebpf_dns_query_latency_bucket`.
- Pod placement/resources: `kube_pod_info`, `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `container_network_receive_bytes_total`, `container_network_transmit_bytes_total`.

Holmes toolsets:

- Prometheus range/instant queries.
- Kubernetes read APIs for node conditions, pod placement, events, endpoints, and logs for specific implicated pods.
- Cilium/Hubble: `cilium_status_verbose`, `cilium_node_list`, `cilium_endpoint_health`, `cilium_endpoint_list`, `hubble_observe_flows_summary`, `hubble_observe_drops`, `hubble_list_nodes`.

## NetworkDoctor eBPF metric reference (verified)

The agent exports these names (checked in Prometheus on the on-prem lab, 2026-09-28). Use them as written; do not guess suffixes.

- Counters: `ebpf_tcp_retransmits_total`, `ebpf_tcp_retransmit_bursts_total`, `ebpf_tcp_connect_attempts_total`, `ebpf_tcp_connect_failed_total`, `ebpf_tcp_connections_short_lived_total`, `ebpf_tcp_connections_closed_total`, `ebpf_dns_queries_total`, `ebpf_dns_responses_total`, `ebpf_dns_rcode_errors_total`, `ebpf_dns_rcode_total{rcode}`, `ebpf_dns_slow_total`, `ebpf_udp_packets_total`, `ebpf_udp_long_flows_total`.
- Gauges: `ebpf_tcp_connections_active`, `ebpf_tcp_connections_time_wait`.
- Histograms (`_bucket`/`_sum`/`_count`, `le` in powers of two): `ebpf_tcp_srtt_microseconds` (µs), `ebpf_tcp_cwnd` (segments), `ebpf_dns_query_latency` (µs), `ebpf_runqlat` (µs), `ebpf_udp_session_duration` (µs).
- Granularity: every `ebpf_*` series is **per node only** (labels `node`, `instance`, `pod` of the agent). There are no per-flow, per-pod or per-service labels such as `src_pod`/`dst_pod`; use Hubble or application metrics for flow-level attribution.
- There is no DNS timeout counter. `ebpf_dns_slow_total` counts responses slower than the agent threshold (default 50 ms) and is the closest proxy.
- Convert µs histograms before comparing to seconds: `histogram_quantile(0.99, sum by (le, node) (rate(ebpf_runqlat_bucket[5m]))) / 1e6`.

## Workflow

1. Identify affected and peer nodes.
   - Extract affected node from alert labels.
   - If absent, infer nodes with the highest abnormal network latency, retransmission, drops, DNS timeouts, softnet drops, or conntrack failures.
   - Select peer nodes with similar labels.

2. Confirm node-locality.
   - Compare the same metrics across affected and peer nodes.
   - Verify that multiple pods or flows on the affected node are degraded.
   - If only one workload is affected and peers hosting the same workload are normal, consider Rule 7 or app-specific failure.

3. Check node conditions and events.
   - Read node status conditions: Ready, MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable.
   - Read recent events for the affected node around `alert_time`.
   - Record taints, cordon state, and recent kubelet/CNI events if exposed.

4. Evaluate resource pressure.
   - Query CPU modes, load, run queue latency, memory availability, major faults, disk iowait, and disk IO time.
   - Identify whether resource pressure precedes network symptoms.

Example PromQL patterns:

```promql
sum by (instance, mode) (rate(node_cpu_seconds_total{instance=~"$instance"}[5m]))

histogram_quantile(
  0.99,
  sum by (le, instance) (rate(ebpf_runqlat_bucket{instance=~"$instance"}[5m]))
)

rate(node_disk_io_time_seconds_total{instance=~"$instance"}[5m])
```

5. Evaluate network stack pressure.
   - Query softnet drops/times squeezed, NIC drops/errors, TCP retransmissions/RTT, DNS timeouts, and conntrack ratio/insert failures.
   - Compare against peers and baseline.

6. Inspect pod distribution and noisy neighbors.
   - List pods on the affected node.
   - Identify top CPU, memory, and network consumers.
   - Look for recent rollouts, restarts, or pod bursts on the affected node.

7. Inspect Cilium/Hubble node health.
   - Check Cilium status and endpoint health.
   - Use Hubble node list and flow summaries if available.
   - Look for drops concentrated on the affected node.

8. Classify the node-local failure.
   - CPU/run queue pressure.
   - Disk/iowait or host resource pressure.
   - SoftIRQ/NIC pressure.
   - Conntrack pressure.
   - Cilium/node datapath health issue.
   - Noisy workload concentrated on one node.
   - Inconclusive node-local symptom.

## Evidence and Exclusion Logic

Confirm node-local failure when:

- The affected node deviates materially from peer nodes in node/network metrics.
- Multiple pods or flows on the node show impact.
- Timing aligns between node-level abnormal signal and user-visible/network symptoms.

Exclude node-local primary cause when:

- The same symptom appears evenly across many nodes.
- Only one workload is affected regardless of node.
- CoreDNS, NetworkPolicy, or conntrack evidence explains the issue more directly.

Classify noisy neighbor when:

- One or a small number of pods on the node drive CPU/network/connection churn.
- Node-level symptoms follow that workload's change.

## Stop Conditions

Stop when:

- Node locality is confirmed and the likely node-level class is identified.
- Node locality is excluded by cross-node comparison.
- Required node metrics are unavailable and Kubernetes/Cilium status cannot provide an alternative.

Do not expand to all cluster logs. Keep pod log reads limited to specifically implicated workloads.

## Output Schema

Every NetworkDoctor skill returns the same top-level structure so the backend can parse any result the same way. Put scenario-specific findings under `scenario_details`, never as new top-level keys. Return it as a single fenced YAML block, after any prose summary.

```yaml
rule_id: "rule-6"
scenario: "node-localized-failure"
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
scenario_details: {}   # optional; for this scenario: `affected_node`, `peer_nodes`, `classification`, `cross_node_comparison`, `implicated_workloads`
```

## Safety and Read-Only Constraints

- Do not cordon, drain, reboot, restart, scale, or reschedule workloads.
- Do not change Cilium, kubelet, sysctl, or node configuration.
- Do not run intrusive diagnostics or packet captures.
- Do not fetch Secrets, tokens, private keys, or kubeconfigs.
- Use read-only metrics, Kubernetes reads, scoped logs, and Hubble/Cilium observations only.

