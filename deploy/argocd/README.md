# Argo CD (on-prem lab): app-of-apps

Layout mirrors a bootstrap/apps repo:

```text
deploy/
  argocd/root-application.yaml    the ONE Application registered by hand
  bootstrap/                      root chart: renders child Applications
    templates/_application.yaml   applications[] -> Application CRs
    onprem-lab.yaml               environment file: destination, repo, app list
  helm/networkdoctor/             child chart (agent, backend, PrometheusRule, AlertmanagerConfig)
  helm/networkdoctor-demo/        child chart (Cat Shop + fortio)
```

Sync is manual at every level. Argo CD shows drift; a person applies it.

## Register the root (once)

```bash
kubectl apply -f https://raw.githubusercontent.com/weeeeestern/NetworkDoctor/main/deploy/argocd/root-application.yaml
```

In the UI, `networkdoctor-root` appears OutOfSync. Sync it: that creates (or
adopts) the child Applications `networkdoctor` and `networkdoctor-demo`.
Then open each child and sync it separately.

Adoption note: an Application created earlier by hand with the same name
(`networkdoctor`) is simply taken over by the root sync. Application CRs have
no immutable fields, so the workloads keep running; only the owner changes.

## Adding a child app

1. Create the chart under `deploy/helm/<name>/`.
2. Add `- name: <name>` (plus `namespace`, `valueFiles` if needed) to
   `deploy/bootstrap/onprem-lab.yaml`.
3. Commit to `main`, sync the root (new child appears), sync the child.

## Verify after the child sync

```bash
kubectl -n networkdoctor get ds,deploy,svc,servicemonitor,prometheusrule,alertmanagerconfig
kubectl -n networkdoctor-demo get deploy,svc,servicemonitor
curl -sG 'http://192.168.0.17:30090/api/v1/query' --data-urlencode 'query=up{job=~"networkdoctor-agent|catshop"}'
curl -sG 'http://192.168.0.17:30090/api/v1/query' --data-urlencode 'query=increase(ebpf_udp_packets_total[5m])'
```

## Rollback

Sync a previous revision from the child's History tab, or revert the commit
on `main` and sync. Deleting a child Application (it carries the resources
finalizer) deletes its workloads; deleting the root does not cascade.

## History

Before the root chart existed, `networkdoctor` and `networkdoctor-demo` were
registered from standalone manifests in this directory. Those files were
removed once the root took over; the rendered children are equivalent.
