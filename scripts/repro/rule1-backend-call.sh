#!/usr/bin/env bash
# Rule 1 · network-congestion, through a service-to-service call
#
# rule1-congestion.sh puts the loss on the client side, outside the time Cat Shop
# records, so it needs /delay/1 to lift p99. Real services call each other: a
# frontend that waits on a backend records that wait in its own
# http_request_duration_seconds. This script reproduces that shape without
# /delay:
#
#   fortio --> [front: podinfo --backend-url=catshop/echo] --(30% loss)--> catshop
#
# The front pod runs on a worker without Cat Shop, so every retransmit on that
# node comes from the front. Its p99 and the node's retransmits both rise and
# Rule 1 fires on the same node (2026-11-01: pending after 52s, firing after 3m52s).
# Needs the networkdoctor-demo chart (Cat Shop) and kube-prometheus-stack.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-18}
LOSS=${ND_LOSS:-30%}
WARMUP=${ND_WARMUP:-90}                    # seconds of clean traffic before the loss
SM_RELEASE=${ND_SM_RELEASE:-prometheus-stack}  # label kube-prometheus-stack selects ServiceMonitors by
PODINFO_IMAGE=${ND_PODINFO_IMAGE:-ghcr.io/stefanprodan/podinfo:6.15.0}
FORTIO_IMAGE=${ND_FORTIO_IMAGE:-fortio/fortio:1.69.5}
FRONT=nd-r1b-front
LOAD=nd-r1b-load

nd_preflight
nd_backend_connect
nd_require_quiet "rule-1"

CATSHOP_NODES=$(kubectl -n "$ND_DEMO_NS" get pod \
  -l app.kubernetes.io/name=catshop,app.kubernetes.io/component=app \
  -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u)
[[ -n "$CATSHOP_NODES" ]] || nd_die "no Cat Shop pod found in $ND_DEMO_NS"
NODE=${ND_NODE:-$(nd_workers | grep -vxF "$CATSHOP_NODES" | head -1)}
[[ -n "$NODE" ]] || nd_die "every worker runs Cat Shop; set ND_NODE to a node without it"
nd_log "front node=$NODE loss=$LOSS backend=$ND_CATSHOP_URL/echo duration=${DURATION}s"

front_x() { kubectl -n "$ND_DEMO_NS" exec "$FRONT" -c net -- sh -c "$1"; }

# Front pod: podinfo forwards POST /echo to Cat Shop and waits for the answer.
# The netshoot container shares the pod network, so tc on its eth0 only slows
# the front's own traffic.
ND_PODS+=("$FRONT" "$LOAD")
kubectl apply -f - >/dev/null <<EOF || nd_die "could not create the front"
apiVersion: v1
kind: Pod
metadata:
  name: $FRONT
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true", app: $FRONT}
spec:
  nodeName: $NODE
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  tolerations: [{operator: Exists}]
  containers:
  - name: front
    image: $PODINFO_IMAGE
    command: ["./podinfo", "--port=9898", "--port-metrics=9797", "--backend-url=$ND_CATSHOP_URL/echo"]
    ports: [{name: http, containerPort: 9898}, {name: http-metrics, containerPort: 9797}]
  - name: net
    image: $ND_TOOL_IMAGE
    command: ["sleep", "3600"]
    securityContext: {capabilities: {add: ["NET_ADMIN"]}}
---
apiVersion: v1
kind: Service
metadata:
  name: $FRONT
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true", app: $FRONT}
spec:
  selector: {app: $FRONT}
  ports:
  - {name: http, port: 9898, targetPort: http}
  - {name: http-metrics, port: 9797, targetPort: http-metrics}
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: $FRONT
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true", release: $SM_RELEASE}
spec:
  namespaceSelector: {matchNames: [$ND_DEMO_NS]}
  selector: {matchLabels: {app: $FRONT}}
  endpoints:
  - port: http-metrics
    path: /metrics
    interval: 15s
    relabelings:
    - {action: replace, sourceLabels: [__meta_kubernetes_pod_node_name], targetLabel: node}
EOF
# The Service and ServiceMonitor are not pods, so nd_cleanup would not remove them.
nd_on_exit "kubectl -n '$ND_DEMO_NS' delete servicemonitor,service '$FRONT' --ignore-not-found"
kubectl -n "$ND_DEMO_NS" wait --for=condition=Ready "pod/$FRONT" --timeout=180s >/dev/null \
  || nd_die "front pod not Ready"

# Start the load only once the front answers: fortio exits if its first
# requests fail. OnFailure restarts it if the Service endpoint is still settling.
kubectl apply -f - >/dev/null <<EOF || nd_die "could not create the load pod"
apiVersion: v1
kind: Pod
metadata:
  name: $LOAD
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true", app: $LOAD}
spec:
  restartPolicy: OnFailure
  terminationGracePeriodSeconds: 1
  containers:
  - name: fortio
    image: $FORTIO_IMAGE
    args: ["load", "-qps", "10", "-c", "4", "-t", "0", "-payload", "hi", "http://$FRONT.$ND_DEMO_NS.svc:9898/echo"]
EOF
kubectl -n "$ND_DEMO_NS" wait --for=condition=Ready "pod/$LOAD" --timeout=120s >/dev/null \
  || nd_die "load pod not Ready (kubectl -n $ND_DEMO_NS logs $LOAD)"

nd_log "clean traffic for ${WARMUP}s so Prometheus has a baseline"
sleep "$WARMUP"

front_x "tc qdisc replace dev eth0 root netem loss $LOSS" || nd_die "tc netem failed"
nd_on_exit "front_x 'tc qdisc del dev eth0 root'"

nd_truth rule-1 node "$NODE"
nd_truth rule-1 client "$FRONT"
nd_truth rule-1 service "$FRONT"
SINCE=$(nd_now)
nd_fault_window "$DURATION"

nd_wait "$SINCE" "rule-1" "$WAIT_MIN"
