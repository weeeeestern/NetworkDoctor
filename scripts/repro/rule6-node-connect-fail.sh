#!/usr/bin/env bash
# Rule 6 · node-localized-failure
# TCP connect failures concentrate on one node while the others stay normal.
#
# A client pod on ND_NODE opens TCP connections to a closed port on a target
# pod. Every attempt is refused, so ND_NODE's connect failure ratio goes above
# 10% and no other node changes.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-300}
WAIT_MIN=${ND_WAIT_MIN:-14}
PORT=${ND_CLOSED_PORT:-9}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-6"
NODE=$(nd_node)
PEER=$(nd_peer_node "$NODE")

nd_pod nd-r6-target "$PEER" pod
TARGET_IP=$(kubectl -n "$ND_DEMO_NS" get pod nd-r6-target -o jsonpath='{.status.podIP}')
nd_pod nd-r6-client "$NODE" pod
nd_log "failing node=$NODE target=$TARGET_IP:$PORT on $PEER duration=${DURATION}s"

nd_truth rule-6 node "$NODE"
for c in refused "closed port" "port $PORT" nd-r6-client RST; do nd_truth rule-6 cause "$c"; done
SINCE=$(nd_now)
nd_fault_window "$DURATION"
nd_bg nd-r6-client "$DURATION" "
  for i in 1 2; do
    ( while [ \$(date +%s) -lt \$end ]; do
        curl -s -o /dev/null --connect-timeout 1 http://$TARGET_IP:$PORT/; sleep 0.25
      done ) &
  done; wait"

nd_wait "$SINCE" "rule-6" "$WAIT_MIN"
