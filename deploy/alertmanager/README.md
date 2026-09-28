# Alertmanager routing to the NetworkDoctor backend

Alertmanager owns grouping, deduplication and retry delivery. The backend
only needs one route that forwards every alert carrying `source="networkdoctor"`
to `POST /webhooks/alertmanager`.

Pick the file that matches how Alertmanager is managed on the cluster:

| File | When |
| --- | --- |
| `alertmanager-route.example.yaml` | Raw `alertmanager.yml` (Secret/ConfigMap managed by hand) |
| `alertmanagerconfig.example.yaml` | Prometheus Operator with the `AlertmanagerConfig` CRD |
| `kube-prometheus-stack-values.example.yaml` | kube-prometheus-stack Helm values (`alertmanager.config`) |

Webhook URL convention (release `networkdoctor`, namespace `networkdoctor`):

    http://networkdoctor-backend.networkdoctor.svc:8080/webhooks/alertmanager

The backend is idempotent on `fingerprint + startsAt`, so redeliveries on
`group_interval` / `repeat_interval` update the same incident. `send_resolved`
must stay `true` so incidents get closed.
