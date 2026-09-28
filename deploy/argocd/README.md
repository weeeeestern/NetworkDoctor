# Argo CD (on-prem lab)

One `Application` (`networkdoctor-application.yaml`) renders the Helm chart
at `deploy/helm/networkdoctor` with `values-onprem-lab.yaml`. Sync is manual:
Argo CD shows drift, a person applies it.

## Before the first sync: remove the hand-applied objects

The cluster already runs the agent from raw manifests
(`~/k8s-network-doctor/*.yaml` on the control plane). They use the same names
the chart will produce (`networkdoctor-agent`), and two of them have immutable
fields (DaemonSet `spec.selector`, Service `spec.clusterIP`) that differ from
the chart. Argo CD cannot adopt them; the sync would fail with
"field is immutable". Delete them first (about one minute of missing metrics):

```bash
kubectl -n networkdoctor delete servicemonitor networkdoctor-agent
kubectl -n networkdoctor delete service networkdoctor-agent
kubectl -n networkdoctor delete daemonset networkdoctor-agent
kubectl -n networkdoctor get all        # expect: No resources found
```

## Register the Application

```bash
kubectl apply -f deploy/argocd/networkdoctor-application.yaml
kubectl -n argocd get application networkdoctor     # SYNC STATUS: OutOfSync, HEALTH: Missing
```

Open Argo CD (`https://192.168.0.17:32163` or via Tailscale
`https://100.122.175.35:32163`), log in as `admin`, open the `networkdoctor`
app. Review the diff (App Diff), then **Sync** → **Synchronize**. Nothing is
applied until that click.

CLI alternative once the `argocd` binary is installed:

```bash
argocd login 192.168.0.17:32163 --insecure --username admin
argocd app diff networkdoctor
argocd app sync networkdoctor
```

## Verify after sync

```bash
kubectl -n networkdoctor get ds,svc,servicemonitor,prometheusrule
kubectl -n networkdoctor logs ds/networkdoctor-agent --tail=5
# Prometheus (NodePort 30090): targets up and the TC-path counter moving
curl -sG 'http://192.168.0.17:30090/api/v1/query' --data-urlencode 'query=up{job="networkdoctor-agent"}'
curl -sG 'http://192.168.0.17:30090/api/v1/query' --data-urlencode 'query=increase(ebpf_udp_packets_total[5m])'
curl -s 'http://192.168.0.17:30090/api/v1/rules' | grep -o 'NetworkDoctor[A-Za-z]*' | sort -u
```

`increase(ebpf_udp_packets_total[5m])` must be well above zero on every
worker. It was near zero with the old image, which is the sign that the TC
hook was not seeing host traffic.

## Later changes

Every Git change to `deploy/helm/networkdoctor/**` on the tracked branch makes
the app OutOfSync. Nothing happens until someone syncs. To roll back, sync a
previous revision from the History tab, or revert the commit and sync.

`targetRevision` is `main`, so a change is deployable only after its PR is
merged. That is deliberate: the branch that Argo CD watches is the branch
that review protects.
