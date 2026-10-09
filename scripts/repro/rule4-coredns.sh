#!/usr/bin/env bash
# Rule 4 · coredns-degradation
# CoreDNS p99 latency goes above 250ms.
#
# Adds a 400ms netem delay on the host-side veth of the CoreDNS pod that runs
# on ND_NODE, then sends uncached lookups through the cluster DNS Service.
#
# With Cilium (legacy host routing), traffic reaches a pod through its own
# lxc* device, not cilium_host, so the device is looked up from the Cilium
# endpoint list. Without Cilium it falls back to "ip route get <pod ip>".
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-480}
WAIT_MIN=${ND_WAIT_MIN:-16}
DELAY=${ND_DNS_DELAY:-400ms}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-4"
NODE=$(nd_node)
COREDNS_IP=$(kubectl -n kube-system get pod -l k8s-app=kube-dns \
  --field-selector "spec.nodeName=$NODE" -o jsonpath='{.items[0].status.podIP}')
[[ -n "$COREDNS_IP" ]] || nd_die "no CoreDNS pod on $NODE (set ND_NODE)"
DNS_IP=$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')

nd_pod nd-r4-node "$NODE" host
CILIUM=$(kubectl -n kube-system get pod -l k8s-app=cilium \
  --field-selector "spec.nodeName=$NODE" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
DEV=""
if [[ -n "$CILIUM" ]]; then
  DEV=$(kubectl -n kube-system exec "$CILIUM" -c cilium-agent -- cilium-dbg endpoint list -o json 2>/dev/null \
    | python3 -c "
import json, sys
for e in json.load(sys.stdin):
    n = e.get('status', {}).get('networking', {})
    if '$COREDNS_IP' in [a.get('ipv4') for a in n.get('addressing', [])]:
        print(n.get('interface-name', ''))")
fi
[[ -n "$DEV" ]] || DEV=$(nd_x nd-r4-node "ip route get $COREDNS_IP | grep -oE 'dev [^ ]+' | cut -d' ' -f2")
[[ -n "$DEV" ]] || nd_die "could not find the host device of CoreDNS $COREDNS_IP"
nd_log "node=$NODE coredns=$COREDNS_IP dev=$DEV delay=$DELAY dns=$DNS_IP duration=${DURATION}s"

nd_x nd-r4-node "tc qdisc replace dev $DEV root netem delay $DELAY"
nd_on_exit "nd_x nd-r4-node 'tc qdisc del dev $DEV root'"

COREDNS_POD=$(kubectl -n kube-system get pod -l k8s-app=kube-dns \
  --field-selector "spec.nodeName=$NODE" -o jsonpath='{.items[0].metadata.name}')
nd_truth rule-4 node "$NODE"
nd_truth rule-4 pod "$COREDNS_POD"
nd_pod nd-r4-client "$NODE" pod
SINCE=$(nd_now)
nd_fault_window "$DURATION"
nd_bg nd-r4-client "$DURATION" "
  for i in 1 2 3 4 5 6; do
    ( while [ \$(date +%s) -lt \$end ]; do
        dig +tries=1 +time=3 @$DNS_IP \"r\$RANDOM\$RANDOM.wikipedia.org\" >/dev/null
      done ) &
  done; wait"

nd_wait "$SINCE" "rule-4" "$WAIT_MIN"
