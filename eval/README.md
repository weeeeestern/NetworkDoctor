# RCA evaluation

This directory measures how often NetworkDoctor names the right root cause, and
compares investigation architectures on the same incidents.

| Architecture | What Holmes gets | Extra cost per incident |
| --- | --- | --- |
| `baseline` | Alert, labels, 6 eBPF evidence queries | none |
| `derived` | Baseline plus deterministic facts computed by the backend | a few PromQL queries |
| `derived+jev` | Derived, then a Jev consistency check; one re-examination if it fails | one Jev call, sometimes a second Holmes call |

The derived facts are numbers the model misread in earlier runs, computed in
code instead:

| Scenario | Fact |
| --- | --- |
| coredns-degradation | Per-pod p99 and the slowest/fastest ratio at similar load (ratio 2 or more points at one pod) |
| network-congestion | When latency and retransmits crossed their thresholds, the onset lag, their correlation, and pods started on the retransmitting node before the alert |
| conntrack-exhaustion, dns-conntrack-correlation | Whether `nf_conntrack_entries_limit` changed during the window |
| node-localized-failure | The failing node's connect failure ratio and onset, whether failures come without retransmits (refused/reset) or with them (dropped), and pods started on that node just before the failures |

## How a run is scored

The fault is known because `scripts/repro` injected it. A run is **correct**
when all three hold:

1. **Routed:** the answer's `scenario` is the expected skill.
2. **Status ok:** `investigation_status` is in the accepted set.
3. **Cause ok:** `root_cause` names the injected fault, for example the delayed CoreDNS pod or its node for Rule 4.

The templates are in `expectations.json`. Matching is case-insensitive and
starts at a word boundary, so `RST` does not match `first`.

## Modes

**Replay** re-investigates stored incidents from `cases.json` against the same
Prometheus history. It is cheap and repeatable. Kubernetes objects from the
original runs are gone, which affects every architecture equally. Prometheus
keeps 10 days, so the 2026-10-01/02 corpus is usable until about 2026-10-11.

**Live** runs one repro script, holds the fault, and investigates the new
incident with every architecture while the fault is still running. The repro
script writes its ground truth (node, pod, policy) to a file the harness reads.

```bash
# on the lab control plane, from a copy of the repo
python3 eval/run.py replay --archs baseline,derived,derived+jev
python3 eval/run.py replay --archs baseline,derived --only rule-4,rule-1-cross --repeat 3
python3 eval/run.py live scripts/repro/rule4-coredns.sh --archs baseline,derived
python3 eval/run.py report eval/results/*.jsonl > eval/report.md
```

Evaluation runs go through `POST /eval/runs` on the backend. They use the same
code path as automatic investigations and never change the stored incident;
each outcome is saved under `<data-dir>/_eval/`.

## Cost and time

Each run is one Holmes investigation: about 2 to 3 minutes and about $0.04 with
`gateway-luna`. The full corpus has 15 cases, so 3 architectures are 45 runs,
roughly 2 hours and $2. Runs are serialized (one evaluation at a time).

## Jev

`derived+jev` needs `backend.jev.enabled=true` and a Secret with the API key.
Without the key the backend logs a warning and the harness skips that
architecture.

```bash
kubectl -n networkdoctor create secret generic jev-api --from-literal=JEV_API_KEY=<key>
```

Jev is a SaaS endpoint (`api.typesafe.ai`). Air-gapped clusters cannot use it;
the checker sits behind an interface so it can be replaced or turned off.

## Reading the result honestly

- 15 cases is a small sample. Report counts, not just percentages, and repeat runs (`--repeat`) before claiming a difference.
- The derived facts were designed after seeing Rule 1 and Rule 4 fail, so those scenarios favour `derived`. The other scenarios are the control.
- Replay cannot see deleted Kubernetes objects. Use live runs for the final numbers.
