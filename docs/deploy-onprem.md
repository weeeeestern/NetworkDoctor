# NetworkDoctor on-prem deployment (MVP)

This guide covers the first end-to-end milestone:

```text
agent (DaemonSet) -> Prometheus scrape -> smoke alert rule -> Alertmanager
  -> backend POST /webhooks/alertmanager -> incident JSON
```

HolmesGPT, evidence collection and reports come after this path is proven.

## Responsibility split

| Layer | Owns | Does not own |
| --- | --- | --- |
| Prometheus rules | firing / resolved decision | routing, incidents |
| Alertmanager | grouping, dedup, retry delivery | detection |
| networkdoctor-backend | incident record, idempotency, later evidence/Holmes/report | any duplicate detection logic |

## 0. Discovery (run before choosing values)

Everything marked VERIFY in `deploy/helm/networkdoctor/values.yaml` comes
from these checks. Record the answers; they decide which chart toggles to
enable.

Cluster and CRDs:

```bash
kubectl version -o yaml
kubectl get nodes -o wide
kubectl get crd | grep -E 'servicemonitors|podmonitors|prometheusrules|alertmanagerconfigs|cilium|hubble|argoproj'
kubectl get storageclass
```

Prometheus / Alertmanager / Grafana:

```bash
kubectl get pods -A | grep -iE 'prometheus|alertmanager|grafana'
kubectl get svc  -A | grep -iE 'prometheus|alertmanager|grafana'
kubectl get servicemonitor,podmonitor,prometheusrule -A
helm list -A
```

Worker NIC and kernel:

```bash
for h in worker01@192.168.0.18 worker02@192.168.0.19 worker03@192.168.0.20; do
  ssh "$h" 'hostname; ip route get 1.1.1.1 | head -1; uname -r; test -r /sys/kernel/btf/vmlinux && echo BTF_OK || echo BTF_MISSING'
done
```

The interface printed by `ip route get 1.1.1.1` (`dev <name>`) is the value
for `agent.iface`. If the three workers differ, either install per node class
with different values or start with the interface used by the node you test
first and extend later.

Cilium / Hubble (only matters for Rule 8 later, not for the smoke test):

```bash
kubectl -n kube-system exec ds/cilium -- cilium status | head -30
kubectl -n kube-system get pods,svc | grep -i hubble
```

Image:

```bash
docker manifest inspect docker.io/7910trio/networkdoctor-agent:edge >/dev/null && echo agent-image-ok
```

The backend image is published by `.github/workflows/backend-image.yml` on
the first `main` push that touches it. Until then build it locally:

```bash
docker build -f build/backend/Dockerfile -t docker.io/7910trio/networkdoctor-backend:edge .
docker push docker.io/7910trio/networkdoctor-backend:edge
```

## 1. hostNetwork check on one worker

`internal/bpf/tc.go` resolves `-iface` with `netlink.LinkByName` in the
process's own network namespace. Inside a pod network namespace only the pod
veth (named `eth0`) exists, so a non-hostNetwork pod would attach the TC
programs to the pod veth, not to the host NIC. `privileged: true` grants
capabilities; it does not move the process into the host network namespace.

The chart therefore defaults to `agent.hostNetwork: true`. Confirm it on one
node before the cluster-wide rollout:

```bash
# Render only the DaemonSet, pinned to one node.
helm template nd deploy/helm/networkdoctor -n networkdoctor \
  --set backend.enabled=false \
  --set agent.iface=<NIC from step 0> \
  --set agent.nodeSelector."kubernetes\.io/hostname"=worker01 \
  | kubectl apply -n networkdoctor -f -

kubectl -n networkdoctor logs ds/nd-agent --tail=30
# Expect: "serving Prometheus metrics on :9102/metrics" and no "attach TC programs" error.

# On the worker: the clsact qdisc and the two filters must be on the host NIC.
ssh worker01@192.168.0.18 'tc qdisc show dev <NIC>; tc filter show dev <NIC> ingress; tc filter show dev <NIC> egress'

curl -s http://192.168.0.18:9102/metrics | grep -E '^ebpf_(tcp_connections_active|udp_packets_total)'
# ebpf_udp_packets_total must increase over time if the TC hook sees host traffic.
```

Then remove the test objects (`kubectl delete -n networkdoctor ds nd-agent`)
and proceed with the real install. If the test proves hostNetwork is not
needed, set `agent.hostNetwork=false` and record why in values.yaml.

## 2. Install

The lab installs through Argo CD with manual sync (see
`deploy/argocd/README.md` for the exact sequence, including removal of the
hand-applied DaemonSet that already runs on this cluster). The chart itself
is plain Helm, so a direct install works the same way on a cluster without
Argo CD:

```bash
kubectl create namespace networkdoctor

# Edit deploy/helm/networkdoctor/values-onprem-lab.yaml with the step 0 answers, then:
helm upgrade --install networkdoctor deploy/helm/networkdoctor \
  -n networkdoctor \
  -f deploy/helm/networkdoctor/values-onprem-lab.yaml
```

Chart toggles by discovery result:

| Discovery result | Values |
| --- | --- |
| `servicemonitors.monitoring.coreos.com` exists | `serviceMonitor.enabled=true`, `serviceMonitor.labels` matching the Prometheus CR selector (kube-prometheus-stack: `release: <stack release>`) |
| no ServiceMonitor CRD | keep `false`; add a scrape job for the headless service `networkdoctor-agent.networkdoctor.svc:9102` (or use `kubernetes_sd_configs` on the pod labels `app.kubernetes.io/component=agent`) |
| `prometheusrules.monitoring.coreos.com` exists | `prometheusRule.enabled=true`, `prometheusRule.labels` matching the ruleSelector |
| no PrometheusRule CRD | `ruleConfigMap.enabled=true`, mount the ConfigMap and add it to `rule_files` |

Verify:

```bash
kubectl -n networkdoctor get ds,deploy,svc,servicemonitor,prometheusrule
kubectl -n networkdoctor logs ds/networkdoctor-agent --tail=20
kubectl -n networkdoctor logs deploy/networkdoctor-backend --tail=5

# Prometheus sees the targets and the metrics.
curl -sG 'http://<prometheus>/api/v1/targets' | grep -o '"health":"[a-z]*"' | sort | uniq -c
curl -sG 'http://<prometheus>/api/v1/query' --data-urlencode 'query=up{job=~".*networkdoctor-agent.*"}'
curl -sG 'http://<prometheus>/api/v1/query' --data-urlencode 'query=ebpf_tcp_retransmit_bursts_total'
curl -sG 'http://<prometheus>/api/v1/rules' | grep -o 'NetworkDoctor[A-Za-z]*' | sort -u
```

## 3. Alertmanager route

Apply one of the files in `deploy/alertmanager/` (see its README). Then
verify Alertmanager loaded it:

```bash
curl -s 'http://<alertmanager>/api/v2/status' | grep -o 'networkdoctor-backend'
```

## 4. Prove the webhook path

Fastest check, no alert needed: post a synthetic payload to the backend
from inside the cluster.

```bash
kubectl -n networkdoctor run curl --rm -it --image=curlimages/curl:8.10.1 --restart=Never -- \
  curl -s -X POST http://networkdoctor-backend:8080/webhooks/alertmanager \
  -H 'Content-Type: application/json' \
  -d '{"version":"4","status":"firing","receiver":"networkdoctor-backend","externalURL":"http://am",
       "alerts":[{"status":"firing","fingerprint":"deadbeef00000001",
       "labels":{"alertname":"NetworkDoctorTCPRetransmitBurstHigh","source":"networkdoctor","severity":"warning","rule_id":"smoke-1","scenario":"smoke-tcp-retransmit-burst","node":"worker01","instance":"192.168.0.18:9102"},
       "annotations":{"summary":"synthetic test"},
       "startsAt":"2026-09-28T00:00:00Z","endsAt":"0001-01-01T00:00:00Z"}]}'
# -> {"received":1,"created":1,"updated":0,"incidents":["inc-..."]}
# Repeat the same command: created:0, updated:1, same incident id.

kubectl -n networkdoctor port-forward svc/networkdoctor-backend 8080:8080 &
curl -s localhost:8080/incidents | head -40
```

Real alert: `NetworkDoctorTCPRetransmitBurstHigh` fires on any retransmit
burst. Generate one on a worker, for example with `tc qdisc add dev <NIC>
root netem loss 20%` for a minute against a TCP transfer (remove it with `tc
qdisc del dev <NIC> root`), or lower the burst threshold via
`agent.thresholds.retrans-burst: "1"`. Then:

```bash
curl -s 'http://<alertmanager>/api/v2/alerts?filter=source%3D%22networkdoctor%22'
kubectl -n networkdoctor logs deploy/networkdoctor-backend | grep webhook
```

To exercise Rule 1-8 end to end, including the Holmes investigation, run the
fault scripts in [`scripts/repro/`](../scripts/repro/README.md) on a lab
cluster, one at a time.

## 5. Rollback

Everything is a single Helm release; nothing outside the namespace is
modified except the Alertmanager route you added by hand.

```bash
helm -n networkdoctor uninstall networkdoctor     # removes DS, backend, svc, monitors, rules
# remove the Alertmanager route/receiver from step 3
```

Uninstalling the DaemonSet detaches the TC filters and BPF links (the agent
closes them on SIGTERM). The clsact qdisc stays on the NIC; it is inert.

## 6. Argo CD

App-of-apps: `deploy/argocd/root-application.yaml` is the only object
registered by hand. It renders `deploy/bootstrap` (root chart) with
`onprem-lab.yaml`, which creates one child Application per entry of its
`applications:` list (`networkdoctor`, `networkdoctor-demo`). No level has
`syncPolicy.automated`; a person syncs root and children. See
`deploy/argocd/README.md`.

Helm release name and namespace are the same as a direct `helm install`, so
the chart needs no changes for Argo CD.

## Known gaps in this milestone

- Backend persistence defaults to `emptyDir`; incidents vanish on pod
  restart. Set `backend.persistence.enabled=true` once a StorageClass is
  confirmed.
- `POST /incidents/{id}/holmes` returns 501 until the Holmes milestone.
- Production Rule 1-8 alerts are not included; only smoke rules over
  `ebpf_*` metrics.
- The non-privileged capability path (`agent.privileged=false`) is rendered
  but untested.
