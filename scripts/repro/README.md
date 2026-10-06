# Fault reproduction scripts

These scripts break a lab cluster on purpose, one scenario at a time. Each one
checks that the whole NetworkDoctor chain works end to end:

```
fault → eBPF agent / exporters → Prometheus rule → Alertmanager
      → backend webhook → incident → Holmes investigation (skill) → report
```

> **Lab clusters only.** The scripts lower `nf_conntrack_max`, add `tc netem`
> qdiscs, create NetworkPolicies and run privileged `hostNetwork` pods. Every
> change is undone by an exit trap, including on Ctrl+C. They refuse to run when
> the current kubectl context matches `ND_CONTEXT_DENY` (default `prod`).

## Scripts

| Script | Rule · scenario | What it does | Fault time | Wait |
| --- | --- | --- | --- | --- |
| `rule1-congestion.sh` | 1 · network-congestion | Client pod with 30% egress loss calls Cat Shop `/delay/1` from 16 loops. `ND_RULE1_MODE=cross` runs the client on a node without Cat Shop to fire the cross-node variant | 7 min | 18 min |
| `rule2-8-nic-drop-policy.sh` | 2 · kernel-network-bottleneck | Peer node sends 50 frames/s with an unused EtherType to the node's NIC, which counts them in `rx_dropped` | 9 min | 16 min |
| | 8 · networkpolicy-misconfiguration | Deny-all ingress policy on a target pod while a client keeps sending it UDP | | |
| `rule3-5-conntrack-dns.sh` | 3 · conntrack-exhaustion | Holds conntrack usage at 85% of a lowered `nf_conntrack_max` | 8 min | 18 min |
| | 5 · dns-conntrack-correlation | Host-level uncached lookups to 1.1.1.1 (slow DNS) on the same node | | |
| `rule4-coredns.sh` | 4 · coredns-degradation | 400ms netem on the CoreDNS pod's host veth plus uncached lookups through kube-dns | 8 min | 16 min |
| `rule6-node-connect-fail.sh` | 6 · node-localized-failure | Client on one node connects to a closed port, so only that node fails | 5 min | 14 min |
| `rule7-app-5xx.sh` | 7 · application-induced-network-bottleneck | ~10 req/s to Cat Shop `/status/500` while the network is quiet | 7 min | 14 min |
| `incidents.sh` | — | Read-only: list incidents, rerun the check, print a report | — | — |

`lib.sh` holds the shared helpers. Every script sources it.

## Requirements

- Run from a host with `kubectl` access to the lab cluster, plus `python3` and `curl`. In the on-prem lab that is the control-plane node.
- The `networkdoctor` chart with the backend, the rules and the AlertmanagerConfig enabled.
- The HolmesGPT app if you want the investigation step to pass.
- The `networkdoctor-demo` chart (Cat Shop) for Rules 1 and 7.
- Cilium with Hubble metrics for Rules 3 (CT map drops) and 8 (`POLICY_DENIED`).
- At least two worker nodes for Rules 2, 6 and 8.

The scripts create their own pods (`nicolaka/netshoot`), labelled
`networkdoctor.io/repro=true`, in `ND_DEMO_NS`. They do not depend on any
hand-made pod in the cluster.

## Usage

Copy the directory to the control-plane node and run one script at a time:

```bash
scp -r scripts/repro cpadmin@<cp>:~/nd-repro
ssh cpadmin@<cp> 'bash ~/nd-repro/rule7-app-5xx.sh'
```

Run them one after another, not in parallel. Faults that overlap on the same
node get correlated into one group, which changes the result.

Leave time between two runs of the same script. Its alerts stay firing for
5-15 minutes after the fault ends, and a new fault would merge into them
without creating a new incident. Even after they resolve, the previous run's
metrics stay inside Holmes' 15-minute lookback and look like a signal that was
"already elevated". The scripts refuse to start until the expected rules have
been resolved for `ND_QUIET_MIN` minutes (default 15).

Each script ends with a table, one line per expected rule:

```
== NetworkDoctor repro result (since 2026-10-01T06:16:37Z)
  rule-3  PASS   inc-3685084427ff  holmes=done  skill=conntrack-exhaustion (expected conntrack-exhaustion)  status=inconclusive conf=high calls=44
  rule-5  PASS   inc-df350ba6a006  holmes=done via inc-3685084427ff  skill=conntrack-exhaustion (expected dns-conntrack-correlation)  ...
```

| Verdict | Meaning |
| --- | --- |
| `PASS` | An incident exists, the investigation finished, and Holmes used the expected skill. A grouped incident passes through its group's primary. |
| `WARN` | The investigation finished, but the answer names a different scenario, or the incident was grouped under a rule this script did not expect. |
| `WAIT` | No incident yet, or the investigation is still queued or running when the wait ended. |
| `FAIL` | The investigation failed or was skipped. |

The exit code is 0 when no rule is left at `WAIT` or `FAIL`.

To read a report afterwards:

```bash
bash ~/nd-repro/incidents.sh                      # incidents from the last hour
bash ~/nd-repro/incidents.sh report inc-3685084427ff
```

## Settings

All are optional environment variables.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ND_NODE` | a worker that runs CoreDNS | Node to break |
| `ND_PEER_NODE` | another worker | Rule 2 frame sender and the remote target pods |
| `ND_IFACE` | the agent's `-iface` argument | Host NIC for Rule 2 |
| `ND_NS` | `networkdoctor` | NetworkDoctor release namespace |
| `ND_DEMO_NS` | `networkdoctor-demo` | Namespace for repro pods |
| `ND_CATSHOP_URL` | `http://catshop.<demo ns>.svc:9898` | Cat Shop base URL |
| `ND_TOOL_IMAGE` | `nicolaka/netshoot:v0.16` | Image for repro pods |
| `ND_DURATION` | per script | Fault duration in seconds |
| `ND_WAIT_MIN` | per script | Minutes to wait for investigations |
| `ND_CONTEXT_DENY` | `prod` | Regex of kubectl contexts to refuse |
| `ND_QUIET_MIN` | `15` | Minutes an earlier incident of the same rule must be resolved |
| `ND_TRUTH_FILE` | unset | Write what was broken (node, pod, policy) for `eval/run.py` |
| `ND_HOLD_FILE` | unset | Keep the fault after the wait while this file exists (used by `eval/run.py live`) |

Script-specific settings are `ND_LOSS` and `ND_RULE1_MODE` (Rule 1), `ND_CONNTRACK_PCT` (Rules 3 and 5), `ND_DNS_DELAY` (Rule 4) and `ND_CLOSED_PORT` (Rule 6).

## Notes from the 2026-10 run

- **Rule 3 is usually judged "excluded".** Holmes sees that `nf_conntrack_entries_limit` itself dropped, which is the correct reading of an artificial limit.
- **Rule 4 needs the pod's own veth.** With Cilium legacy host routing, traffic reaches a pod through its `lxc*` device, not `cilium_host`. A netem on `cilium_host` has no effect. The script finds the device through `cilium-dbg endpoint list`.
- **Rules 3 and 5 share a node, so they become one group.** The backend correlates them and calls Holmes once.
- **Rule 1 has two alerts.** The main alert needs latency and retransmits on the same node, and latency carries the Cat Shop pod's node, so the script defaults to a Cat Shop node. The cross-node variant (severity info) covers client-side loss on another node; reproduce it with `ND_RULE1_MODE=cross`.
- **Rule 8 uses UDP on purpose.** Denied TCP connects also count as connect failures and fire Rule 6 on the same node, which then groups with Rule 2.
- **The fault ends after its fault time, even while the script still waits.** An investigation that starts inside the window sees the live fault. Anything left is restored when the script exits.
