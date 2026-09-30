# NetworkDoctor HolmesGPT skills

One `SKILL.md` per production scenario (Rule 1-8). HolmesGPT loads them via
`fetch_skill`; the Holmes pod pulls this directory from Git at start-up
(`deploy/helm/holmesgpt`, init container `fetch-skills`).

| Rule | Directory | Scenario |
| --- | --- | --- |
| 1 | `network-congestion/` | Network congestion |
| 2 | `kernel-network-bottleneck/` | SoftIRQ / NIC bottleneck |
| 3 | `conntrack-exhaustion/` | Conntrack exhaustion |
| 4 | `coredns-degradation/` | CoreDNS degradation |
| 5 | `dns-conntrack-correlation/` | DNS and conntrack correlation |
| 6 | `node-localized-failure/` | Node-localized failure |
| 7 | `application-induced-network-bottleneck/` | Application-induced network bottleneck |
| 8 | `networkpolicy-misconfiguration/` | NetworkPolicy denied traffic |

## Origin and what changed

The investigation workflows are the supplied baseline (2026-09-27 package)
and were not rewritten. Three changes were made on import (2026-09-30):

1. **Names.** Directories and frontmatter `name` follow the scenario names
   above; the rule number stays in each `description`.
2. **eBPF metric names.** The supplied files used names the agent does not
   export (e.g. `ebpf_tcp_retransmissions_total`, `ebpf_runqlat_seconds_bucket`).
   They were replaced with the exported names verified in Prometheus, and
   each skill gained a "NetworkDoctor eBPF metric reference (verified)"
   section stating units (µs) and granularity (per node only).
3. **Output schema.** Every skill now ends with the same top-level structure
   (`rule_id`, `scenario`, `investigation_status`, `confidence`,
   `time_window`, `affected_resources`, `root_cause`, `trigger_evidence`,
   `supporting_evidence`, `excluded_alternatives`, `recommended_actions`,
   `additional_checks`). Scenario-specific fields from the original schema
   move under `scenario_details`.

## Editing

Change a skill, merge to `main`, then restart Holmes so the init container
fetches the new revision:

```bash
kubectl -n holmesgpt rollout restart deploy/holmesgpt-holmes
```

Known data gaps on the lab cluster (skills say "if exported" and fall back):
`cilium_*` agent metrics are not scraped (the existing ServiceMonitor hits
the Envoy port 9964), and Holmes has no Hubble/Cilium toolset enabled, so
Rule 8 relies on `hubble_drop_total` from Prometheus.
