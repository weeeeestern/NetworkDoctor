# NetworkDoctor on-prem deployment

For the complete installation, browser access and Slack setup sequence, start
with [설치부터 UI·Grafana·Slack 연결까지](install-and-access.ko.md). This document
provides the on-prem discovery, collector validation and deployment details.

The current end-to-end pipeline is:

```text
agent (DaemonSet) -> Prometheus scrape -> Rule 1-8 -> Alertmanager
  -> backend POST /webhooks/alertmanager -> incident JSON + evidence
     ├─ Holmes investigation -> Incident UI / Markdown -> browser-generated PDF
     └─ optional Slack: incident parent + analysis completion reply
```

Smoke rules also exist to verify collection and webhook delivery without a
full scenario. Holmes and evidence collection require their configured service
URLs; Slack requires its own bot, Secret and settings. The NetworkDoctor chart
does not install Prometheus, Grafana, Holmes or an LLM gateway for you.

## Responsibility split

| Layer | Owns | Does not own |
| --- | --- | --- |
| Prometheus rules | firing / resolved decision | routing, incidents |
| Alertmanager | grouping, dedup, retry delivery | detection |
| networkdoctor-backend | incident record, idempotency, evidence, investigation, reports and optional Slack delivery | firing / resolved decisions |

Use one deployment owner. The existing lab is managed by Argo CD with manual
sync; do not also run direct Helm upgrades or uninstalls against its workloads.
The direct Helm commands below are for a release managed by Helm. Argo CD
renders Helm templates without creating a Helm-managed release, so absence from
`helm list` alone does not mean NetworkDoctor is absent. Check the existing
Application and its source before choosing a path.

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

Cilium / Hubble (only when the CNI is Cilium; Rule 8 and the CT-map part of
Rule 3 need it, Rules 1-7 do not):

```bash
kubectl -n kube-system exec ds/cilium -- cilium status | head -30
# Which metrics ports does the agent expose? Want prometheus (9962) and hubble-metrics (9965).
kubectl -n kube-system get ds cilium \
  -o jsonpath='{range .spec.template.spec.containers[*].ports[*]}{.name}={.containerPort}{"\n"}{end}'
```

If `prometheus` (9962) is missing, Cilium was installed without agent metrics.
Enable them in Cilium's own Helm values (a CNI change; the agents roll):

```yaml
prometheus:
  enabled: true          # cilium_* on :9962
hubble:
  metrics:
    enabled: [dns, drop, tcp, flow]   # hubble_* on :9965, "drop" is what Rule 8 reads
```

Nothing to set in the NetworkDoctor chart: with `serviceMonitor.enabled=true`
it already creates one PodMonitor on the cilium-agent pods for both ports
(`cilium.enabled`, default true), independent of any Services or
ServiceMonitors the Cilium install created. Remove older
hand-made Cilium/Hubble ServiceMonitors afterwards so series are not scraped
twice.

Images:

The backend UI, Grafana links and Slack integration were merged into `main` in
PR #27. A separate `feat/ux` image build is no longer required. The image
workflows publish `edge` and `main-<sha>` on relevant main changes, and the lab
values pin a commit tag after publication. Use the agent and backend tags in
the current `deploy/helm/networkdoctor/values-onprem-lab.yaml` for the lab; a
different environment should pin its own tested image versions rather than
assume an older tag includes the feature. A source push alone does not sync the
cluster. For private images, configure the namespace's registry Secret through
`agent.imagePullSecrets` and/or `backend.imagePullSecrets`; a registry login on
the developer's PC is not a cluster image-pull credential.

## 1. hostNetwork check on one worker

`internal/bpf/tc.go` resolves `-iface` with `netlink.LinkByName` in the
process's own network namespace. Inside a pod network namespace only the pod
veth (named `eth0`) exists, so a non-hostNetwork pod would attach the TC
programs to the pod veth, not to the host NIC. `privileged: true` grants
capabilities; it does not move the process into the host network namespace.

The chart therefore defaults to `agent.hostNetwork: true`. On a new target,
confirm it on one node before the cluster-wide rollout. Skip this temporary
installation if an agent already runs on that node: a second hostNetwork agent
would compete for port 9102 and the same TC filters. Replace the NIC, node and
worker address below with the discovery results.

The agent does not call the Kubernetes API and disables token mounting. For
this temporary test, use the namespace's default ServiceAccount and render only
the DaemonSet; no extra ServiceAccount, Service or RBAC object is needed.

```bash
kubectl create namespace networkdoctor --dry-run=client -o yaml | kubectl apply -f -

# Example discovered values; replace before applying.
ND_TEST_IFACE=eno1
ND_TEST_NODE=worker01

# Render only the DaemonSet, pinned to one node, using an existing account.
helm template nd deploy/helm/networkdoctor -n networkdoctor \
  --show-only templates/daemonset-agent.yaml \
  --set backend.enabled=false \
  --set serviceAccount.create=false --set serviceAccount.name=default \
  --set-string agent.iface="$ND_TEST_IFACE" \
  --set-string "agent.nodeSelector.kubernetes\\.io/hostname=$ND_TEST_NODE" \
  | kubectl apply -n networkdoctor -f -

kubectl -n networkdoctor rollout status ds/nd-networkdoctor-agent
kubectl -n networkdoctor logs ds/nd-networkdoctor-agent --tail=30
# Expect: "serving Prometheus metrics on :9102/metrics" and no "attach TC programs" error.

# On the worker: the clsact qdisc and the two filters must be on the host NIC.
ssh worker01@192.168.0.18 'tc qdisc show dev eno1; tc filter show dev eno1 ingress; tc filter show dev eno1 egress'

curl -s http://192.168.0.18:9102/metrics | grep -E '^ebpf_(tcp_connections_active|udp_packets_total)'
# ebpf_udp_packets_total must increase over time if the TC hook sees host traffic.
```

Remove the temporary DaemonSet and wait for its pods to terminate before the
real install, so the old and new agents cannot overlap:

```bash
kubectl -n networkdoctor delete ds nd-networkdoctor-agent \
  --cascade=foreground --wait=true --timeout=120s
```

The namespace is retained for the real install. Keep hostNetwork enabled unless
a separate test establishes why it is unnecessary in the target environment.

## 2. Install

The lab installs through Argo CD with manual sync (see
[deploy/argocd/README.md](../deploy/argocd/README.md)). For that environment,
update the Application's source values and sync it manually; do not use the
following direct Helm command. For a new Helm-managed release, copy the lab
values to a local environment file and replace the NIC, selectors, image tags,
Prometheus/Holmes URLs and storage settings from discovery. Do not copy lab IPs
or service names into another cluster unchanged.

```bash
kubectl create namespace networkdoctor --dry-run=client -o yaml | kubectl apply -f -

# Prepare this local file with the step 0 answers before installing.
helm upgrade --install networkdoctor deploy/helm/networkdoctor \
  -n networkdoctor \
  -f onprem-values.yaml
```

Chart toggles by discovery result:

| Discovery result | Values |
| --- | --- |
| `servicemonitors.monitoring.coreos.com` exists | `serviceMonitor.enabled=true`, `serviceMonitor.labels` matching the Prometheus CR selector (kube-prometheus-stack: `release: <stack release>`) |
| no ServiceMonitor CRD | keep `false`; add a scrape job for the headless service `networkdoctor-agent.networkdoctor.svc:9102` (or use `kubernetes_sd_configs` on the pod labels `app.kubernetes.io/component=agent`) |
| `prometheusrules.monitoring.coreos.com` exists | `prometheusRule.enabled=true`, `prometheusRule.labels` matching the ruleSelector |
| no PrometheusRule CRD | `ruleConfigMap.enabled=true`, mount the ConfigMap and add it to `rule_files` |
| Grafana with the dashboard sidecar (kube-prometheus-stack default) | `grafanaDashboards.enabled=true`; the sidecar only watches its own namespace unless the stack sets `grafana.sidecar.dashboards.searchNamespace: ALL` — either set that or `grafanaDashboards.namespace=<monitoring ns>`. Dashboards: `nd-overview` + `nd-rule-1`..`nd-rule-8` (generated by `scripts/grafana/gen_dashboards.py`) |

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

With the Operator and `alertmanagerConfig.enabled=true`, the chart already
creates the route and receiver. Confirm the Alertmanager CR selects the
AlertmanagerConfig; do not also apply the standalone examples for the same
alerts. Without the Operator, or when managing routing separately, use the raw
examples in [deploy/alertmanager/](../deploy/alertmanager/README.md) instead of
the chart-generated route. Then verify Alertmanager loaded the selected route:

```bash
curl -s 'http://<alertmanager>/api/v2/status' | grep -o 'networkdoctor-backend'
```

## 4. Prove the webhook path

Fastest check, no alert needed: post a synthetic smoke payload to the backend
from inside the cluster. This proves incident ingestion and idempotency only.
`smoke-1` is excluded from Slack notifications and, by default, Holmes
investigation. It is not a Slack end-to-end test, and the expected create count
assumes the example has not already been ingested.

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
```

To inspect the embedded UI locally, keep the following command running in its
own terminal, then open `http://localhost:8080/` in that PC's browser:

```bash
kubectl -n networkdoctor port-forward svc/networkdoctor-backend 8080:8080
```

In another terminal, `curl -s http://localhost:8080/incidents` shows the API
records. Stop the port-forward with Ctrl+C when finished. This localhost URL is
not a shareable team address; see the [access guide](install-and-access.ko.md)
for Grafana and shared access.

For real Rule 1-8 alerts, use the [fault reproduction guide](../scripts/repro/README.md)
on a dedicated lab target. Confirm the target, scenario prerequisites and
cleanup procedure, and run one scenario at a time. Do not improvise a `tc`
fault on a shared worker. After the selected scenario fires, inspect:

```bash
curl -s 'http://<alertmanager>/api/v2/alerts?filter=source%3D%22networkdoctor%22'
kubectl -n networkdoctor logs deploy/networkdoctor-backend | grep webhook
```

To verify Slack too, follow the [Slack integration guide](slack-integration.ko.md)
and generate a new eligible non-smoke firing incident after enabling it. Past
incidents are not backfilled. The original post contains the configured Grafana
link; an analysis completion reply contains the configured Incident UI link.

## 5. Rollback

For a Helm-managed release, inspect its history and roll back to a verified
revision rather than uninstalling it. Compare that revision's values first,
especially persistence and claim settings: rolling back to manifests without
the current PVC can remove it too.

```bash
helm -n networkdoctor history networkdoctor
# Replace VERIFIED_REVISION with the chosen revision number from history.
helm -n networkdoctor get values networkdoctor --revision VERIFIED_REVISION
helm -n networkdoctor rollback networkdoctor VERIFIED_REVISION --wait --timeout 5m
```

For an Argo-managed installation, restore the intended source values/revision
and manually sync the Application. Do not run `helm rollback` or `helm uninstall`
against an Argo-owned app. Image and manifest rollback does not restore old
incident contents; back up the data volume separately before risky changes.

Uninstall is a separate, destructive removal operation. The NetworkDoctor chart
can delete its managed backend PVC because `pvc-backend.yaml` has no retention
annotation; the StorageClass/PV reclaim policy determines whether backing data
is also deleted. A separately supplied `existingClaim` is not created or owned
by this chart. Back up incidents, analysis and Slack receipts before removal.
Namespace deletion also removes out-of-band Secrets and claims in that
namespace, so it is not a routine cleanup or rollback step.

The NetworkDoctor chart currently grants no cluster RBAC. It can create Grafana
dashboard ConfigMaps and monitoring objects in a configured namespace outside
its release namespace, so its footprint is not necessarily confined to one
namespace. Holmes is a separate release with its own Kubernetes access and
cluster RBAC; do not remove shared monitoring CRDs, Cilium resources or storage
provisioners as NetworkDoctor cleanup. If routing was added by hand, manage
that route separately. Argo child Applications carry resource finalizers, so
their deletion can remove their workloads too.

On graceful agent shutdown, the SIGTERM cleanup detaches its TC filters and BPF
links. The clsact qdisc remains; do not delete it blindly because other tools
may also use it. Verify cleanup on the test node if shutdown was interrupted.

## 6. Argo CD

App-of-apps: `deploy/argocd/root-application.yaml` is the only object
registered by hand. It renders `deploy/bootstrap` (root chart) with
`onprem-lab.yaml`, which creates one child Application per entry of its
`applications:` list: `local-path-provisioner` (lab only), `networkdoctor`,
`networkdoctor-demo` and `holmesgpt`. No level has `syncPolicy.automated`; a
person syncs root and children. Prepare the Holmes gateway Secret first, make
the lab StorageClass available before backend persistence, and verify each
child after sync. See [deploy/argocd/README.md](../deploy/argocd/README.md).

Argo uses the configured Helm release name for template names, but it owns the
resulting resources through its Application. Preserve that ownership and the
manual sync workflow when changing image tags, browser URLs or Slack settings.

## Current limitations

- Backend persistence defaults to `emptyDir`; incidents vanish on pod
  restart. Set `backend.persistence.enabled=true` once a StorageClass is
  confirmed.
- Holmes investigation and manual reanalysis are implemented. They require a
  working Holmes URL and model/gateway settings; an unconfigured investigation
  path returns 501. Prometheus evidence needs its own configured URL.
- Rule 1-8 and smoke rules are included. They are active only when installed
  and loaded by the selected Prometheus; each scenario also needs its metrics.
- The embedded UI is served at `/`, but browser URLs, network exposure and
  Grafana login are environment-specific. The chart does not add UI login or
  an Ingress. `ui.url` and `grafana.url` create links; they do not expose a Service.
- Slack is opt-in and requires one backend replica, persistence, a bot Secret,
  channel membership and HTTPS egress. See the [Slack guide](slack-integration.ko.md).
- The non-privileged capability path (`agent.privileged=false`) is rendered
  but untested.
