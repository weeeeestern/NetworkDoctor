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

## Ablation: context and skills (`ablation.py`)

`run.py` changes what the backend does. `ablation.py` changes the question
instead: it asks Holmes `/api/chat` directly about a live fault, so you can
remove the alert context or the skills and see what each one contributes.

| Ask style | What Holmes gets |
| --- | --- |
| `full` | What the backend sends: rule labels, node, investigation window, optional related alerts (`--related`) |
| `symptom` | Only an operator-style report about one node (`--symptom`, or a neutral default) |
| `blind` | "Is there any network problem in the cluster right now?" |

An arm is `name=style@endpoint`. Endpoints are `ns/svc` (port-forwarded) or a
URL. `scripts/repro/holmes-variant.sh` starts a Holmes without skills (`up raw`)
or with skills from a branch (`up ref <git-ref>`), with the same model, key and
toolsets as production.

```bash
bash scripts/repro/holmes-variant.sh up raw
python3 eval/ablation.py --script scripts/repro/t4-cpu-decoy.sh \
  --endpoint main=holmesgpt/holmesgpt-holmes \
  --endpoint raw=holmesgpt-raw/holmesgpt-raw-holmes \
  --arm full_skill=full@main --arm full_raw=full@raw \
  --arm symptom_skill=symptom@main --arm symptom_raw=symptom@raw \
  --rule-id rule-1 --scenario network-congestion --alertname NetworkDoctorNetworkCongestion \
  --target 4 --flag cpu='runqlat|cpu' --flag loss='retransmit|packet loss|netem'
bash scripts/repro/holmes-variant.sh down raw
```

With `--script`, the repro script runs with `ND_HOLD_FILE`, so the fault stays
while the arms are asked; the node comes from its ground truth. `--fault-min`
(default 45) caps the fault from the script start: no ask starts after it, a
running ask is cut at it, and the hold is released. Arms take turns,
one attempt each per round, until each has `--target` valid answers or
`--max-attempts` tries. A valid answer has a yaml block, more than 800
characters and at least 5 tool calls; every ask carries BuildAsk's anti-plan
line, because without it gpt-luna ended about half of the direct asks with a plan
and no investigation.

Answers go to `eval/results/ablation-<time>/answers/`, one file per attempt with
the ask and the answer. `--flag` counts keyword hits and is only a hint: in the
2026-10 runs the keywords for "separate" and "merged" both matched the same
answer. Read the answers to decide what each one concluded.

What the 2026-10 runs showed, at 3-4 answers per arm:

- **Context decided the answer.** With a CPU decoy (100% CPU) next to real packet loss, `full` found the loss with and without skills (4/4 and 3/4). `symptom` fell for the decoy with and without skills (0/4 each).
- **Skills made the checks systematic, not the answers more correct.** With skills, every `full` answer looked up run-queue latency and ruled the decoy out with evidence; without skills, 3 of 4 never looked at CPU.
- **Grouped alerts were merged too often**, with skills as well: unrelated faults on the same node and minute were described as one cause.
