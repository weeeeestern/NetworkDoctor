# Slack incident demo

For the complete installation, private image, access URL and PDF walkthrough,
see [Install and access (한국어)](install-and-access.ko.md).
The [Slack integration guide (한국어)](slack-integration.ko.md) covers app setup and delivery behavior in detail.

This demo uses one Slack parent message per individual incident, even when
Holmes investigates several incidents as a group. Analysis completion appears
in that incident's thread. Grafana shows metrics; the Incident UI explains the
result and downloads the latest report as PDF. PDFs are generated in the browser
and are not stored on the backend volume.

## Prepare the lab

- Use a dedicated test cluster and channel. Follow [the lab installation guide](deploy-onprem.md)
  for Prometheus, Alertmanager, Holmes and the NetworkDoctor chart.
- Use the existing Grafana dashboard sidecar with the chart's dashboard ConfigMaps.
  Verify `nd-rule-1` through `nd-rule-8` are visible. A Grafana Operator installer
  is outside this demo scope; this change does not install an Operator or CRDs.
- Keep one backend replica, a persistent incident volume and the chart's `Recreate`
  strategy. The file store is single writer. Helm rejects Slack-enabled configurations
  without persistence or with more than one backend replica.
- Confirm the selected alerts reach the backend webhook. Remove any parallel
  Alertmanager Slack receiver for those same alerts to avoid a second notification.
- Pick browser-accessible Grafana and Incident UI addresses. Internal `.svc` URLs
  are for backend connections and must not be used as public links. This demo does
  not add login or an Ingress; use the existing trusted lab access path.

## Create the Slack bot and Secret

1. Create a Slack app in the demo workspace. Under **OAuth & Permissions**, add
   the bot scope `chat:write`, then install the app to that workspace.
2. Invite the bot to `#incidents` (for example, `/invite @NetworkDoctor`). Copy the
   channel **ID** from the channel details; use the ID rather than `#incidents`.
3. Put the bot token in a Secret in the backend's namespace. Do not put it in a
   values file, a CLI argument, source control or a screenshot. For example, in
   bash, after the namespace exists:

   ```bash
   read -r -s -p 'Slack bot token: ' nd_demo_bot_token
   printf '\n'
   printf '%s' "$nd_demo_bot_token" |
     kubectl -n networkdoctor create secret generic networkdoctor-slack \
       --from-file=bot-token=/dev/stdin
   unset nd_demo_bot_token
   ```

   The chart references an existing Secret; it does not create a token. Rotate or
   repair the Secret separately and restart the backend after a token change.

See Slack's [posting API](https://docs.slack.dev/reference/methods/chat.postMessage/)
for scopes and thread replies and [rate limits](https://docs.slack.dev/apis/web-api/rate-limits/)
for `Retry-After` behavior. No Slack event subscription or incoming webhook is needed.

## Configure and install this feature build

This feature was merged into main in [PR #27](https://github.com/weeeeestern/NetworkDoctor/pull/27).
Use an image containing that change, or build and publish one from a suitable
revision with a unique tag using the existing backend Dockerfile. Override
`backend.image.repository` and `backend.image.tag` below. Check the current lab
values for its pinned image; an older installed image may still need upgrading.
Private images also require a pull Secret in the workload namespace.

Create a local `demo-values.yaml` with real addresses and your image tag:

```yaml
grafana:
  url: "http://GRAFANA_NODE_IP:GRAFANA_NODE_PORT"
ui:
  url: "http://UI_NODE_IP:UI_NODE_PORT"
slack:
  enabled: true
  channelID: "YOUR_CHANNEL_ID"
  existingSecret: networkdoctor-slack
  botTokenKey: bot-token
backend:
  replicaCount: 1
  image:
    repository: YOUR_DEMO_BACKEND_IMAGE
    tag: YOUR_IMAGE_TAG
  persistence:
    enabled: true
```

The placeholders are not working addresses. `ui.url` must be an origin at `/`
(no subpath); `grafana.url` may include its configured subpath. Neither accepts
credentials, query strings or fragments. Empty values hide their links in Slack
and the UI. Values set links only; they do not expose Services or configure DNS.
For NodePort access, first configure the existing Service/exposure and use its
actual reachable node IP and port. Port forwarding works for a demo on one PC,
but a `localhost` link cannot be shared with teammates on other PCs.

```bash
# Direct Helm management only; for an Argo CD app, commit its values and manually Sync instead.
helm lint deploy/helm/networkdoctor \
  -f deploy/helm/networkdoctor/values-onprem-lab.yaml -f demo-values.yaml
helm upgrade --install networkdoctor deploy/helm/networkdoctor \
  -n networkdoctor --create-namespace \
  -f deploy/helm/networkdoctor/values-onprem-lab.yaml -f demo-values.yaml
kubectl -n networkdoctor rollout status deployment/networkdoctor-backend
```

Slack is disabled by default. When enabled, enrollment is saved atomically only
for a newly created, eligible, non-smoke firing incident. Existing history and
incidents first seen as resolved are not backfilled. A new `startsAt` creates a
new incident and parent. The chosen channel is frozen per incident; changing the
configured channel affects only future enrollment.

## Demo walkthrough

1. Open the channel and verify the UI and Grafana addresses from the same PC the
   audience will use. Check both dashboards and metric data before fault injection.
2. Inject one approved scenario using `scripts/repro/` in the dedicated demo target.
   Let that scenario finish before starting another scenario or manual reanalysis.
3. Expect one **Incident detected** parent for each incident. Open **Open Grafana**.
   The URL uses the incident's own node/service and ten minutes around its event
   window. Rule 6 keeps the dashboard's All-node default for comparison; Rule 1
   includes both affected and retransmit nodes when available.
4. When Holmes finishes with a root cause, expect **Analysis ready** in the same
   thread. Shared group analysis is explicitly labelled as related analysis, not
   proof that every member was individually verified. Recovery is each incident's own.
5. Open **View latest report**, read symptom, cause and evidence, and download PDF.
   The report follows the latest available analysis; it is not pinned to the Slack
   message's version. Reanalysis may add a later completion reply to the same thread.
6. Repeat the same webhook if testing delivery: it must not create another parent.
   Run a later scenario with a new start time to create a separate incident.

## Troubleshooting and manual recovery

The backend saves `slack_notification` inside the incident JSON on the data volume.
Receipts and frozen Slack payloads are deliberately excluded from the public detail
API. Inspect the protected volume when diagnosing delivery. Do not post raw incident
files, tokens or payloads into a shared troubleshooting channel.

- `pending` / `retry`: the worker resumes automatically. It scans every two seconds,
  spaces channel calls by at least one second, and backs off 2, 4, 8 seconds up to
  60 seconds for clear non-delivery. A Slack 429 delays that channel by at least
  `Retry-After` (30 seconds if missing or invalid).
- `failed`: repair a clear rejection, such as bot membership, channel or token scope.
- `unknown` / leftover `sending`: Slack may already have accepted the message.
  Timeouts, malformed success replies, HTTP 5xx or a receipt-save failure must not
  cause a blind repost. On restart, `sending` becomes `unknown`.

For `failed` or `unknown`, pause the demo and stop the backend before editing the
store (for Kubernetes, temporarily scale the Deployment to zero). Back up the exact
incident JSON and reconcile the channel/thread manually. If delivery is confirmed,
set that delivery's `state` to `sent`, record the exact Slack message `ts` as a
**string**, and retain its original channel. For a confirmed analysis reply, also
copy its `source_incident_id` and `source_attempt` into the receipt's
`last_source_incident_id` and `last_source_attempt`. If absence is confirmed and
the cause is repaired, reset only that delivery to `pending`, remove
`next_attempt_at` and `last_error_code`, and retain its payload, source and parent
receipt. Restore one replica. Do not delete the whole receipt, invent a parent
timestamp or re-enroll old incidents; those can create duplicate or detached messages.
If delivery cannot be established, keep `unknown` and reconcile manually.

A backend restart marks interrupted Holmes runs as failed and keeps their prior
result. Use **Reanalyze** in the UI to retry manually. The in-memory investigation
queue is not replayed automatically. During running/failed reanalysis, retained
results are labelled **Previous analysis**; they do not trigger a new completion reply.

## Local verification without Slack or faults

```bash
go test ./...
go test -race ./internal/backend/...
go vet ./internal/backend/... ./cmd/networkdoctor-backend
go build ./cmd/networkdoctor-backend
helm lint deploy/helm/networkdoctor
helm lint deploy/helm/networkdoctor -f deploy/helm/networkdoctor/values-onprem-lab.yaml
```

Notification tests use fake senders and loopback HTTP servers. They cover repeat
delivery, parent-first replies, group/late-join results, restart, rate limits,
ambiguous outcomes and failed receipt persistence. They do not send real messages
or verify a customer's Grafana connectivity. The live walkthrough is a separate
check against the chosen demo cluster, channel, bot and image.
