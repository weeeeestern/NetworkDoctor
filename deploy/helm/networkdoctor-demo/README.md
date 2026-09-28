# networkdoctor-demo (Cat Shop)

An instrumented target workload so NetworkDoctor has application metrics to
correlate with its eBPF signals. It is podinfo with a cat face, plus a fortio
load generator.

| Object | Purpose |
| --- | --- |
| `Deployment/catshop` (2 replicas) | podinfo: HTTP on 9898, Prometheus metrics on 9797, gRPC on 9999 |
| `Service/catshop` (NodePort 30898) | UI + metrics + gRPC ports |
| `ServiceMonitor/catshop` | scrapes `/metrics` every 15s; adds a `node` label |
| `Deployment/catshop-loadgen` | fortio, 10 req/s forever against the Service |

## Metrics that become available

- `http_request_duration_seconds_bucket{namespace,service,pod,node,method,path,status}` (the histogram the Rule 1 and Rule 7 skills query first)
- `http_requests_total{...}` (request rate, 5xx rate)
- podinfo runtime metrics (Go GC, goroutines)

Labels `namespace`, `service`, `pod` come from the ServiceMonitor, so the
skill queries `{namespace=~"$namespace", service=~"$service"}` work as
written.

## Injecting application-side faults

These only touch the app; node and CNI signals stay flat, which is exactly
the Rule 7 ("application-induced") shape.

```bash
# latency: every request waits 2 seconds
kubectl -n networkdoctor-demo run f --rm -it --restart=Never --image=fortio/fortio:1.69.5 -- \
  load -qps 10 -t 60s http://catshop:9898/delay/2

# errors: HTTP 500 on every request
kubectl -n networkdoctor-demo run f --rm -it --restart=Never --image=fortio/fortio:1.69.5 -- \
  load -qps 10 -t 60s http://catshop:9898/status/500
```

For network-side faults keep using the netshoot `client` pod and the scripts
on the control plane (`~/cc_9th_observability/observability/scripts/`); point
them at `http://catshop:9898` instead of the old http-echo backend when you
want the app histogram and the eBPF counters to move together.

## Routing note for later

The NetworkDoctor `AlertmanagerConfig` lives in namespace `networkdoctor`,
and the Prometheus Operator adds a `namespace="networkdoctor"` matcher to it.
Alerts derived from this workload will carry `namespace="networkdoctor-demo"`
and will not match that route. Before writing Rule 1/7 alerts on these
metrics, either add a second `AlertmanagerConfig` in this namespace or set
`alertmanagerConfigMatcherStrategy: None` on the Alertmanager CR.
