#!/usr/bin/env bash
# Rule 6 guard · count()==1
# The same closed-port fault on TWO nodes at once must keep Rule 6 silent:
# its expr requires that exactly one node is above the failure ratio, so a
# cluster-wide symptom is deliberately not "node-localized". Phase 2 then
# stops one client, leaving one bad node, and expects the rule to fire —
# proving the phase-1 silence came from the guard, not a broken injection.
#
# Phase 2 doubles as a normal positive Rule 6 run (one incident, one
# investigation), so a full pass yields both a silence proof and a PASS line.
set -uo pipefail
. "$(dirname "$0")/lib.sh"
. "$(dirname "$0")/lib-silence.sh"

PHASE1=${ND_PHASE1_SEC:-360}   # both nodes failing; > rate ramp + for(2m)
WAIT_MIN=${ND_WAIT_MIN:-14}    # phase 2: rate[5m] decay on the stopped node + for(2m)
PORT=${ND_CLOSED_PORT:-9}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-6"
NODE=$(nd_node)
PEER=$(nd_peer_node "$NODE")

# One target pod, two clients on different nodes. Connect failures count on
# the client's node, so both NODE and PEER go above the ratio in phase 1.
nd_pod nd-r6s-target "$PEER" pod
TARGET_IP=$(kubectl -n "$ND_DEMO_NS" get pod nd-r6s-target -o jsonpath='{.status.podIP}')
nd_pod nd-r6s-c1 "$NODE" pod
nd_pod nd-r6s-c2 "$PEER" pod
nd_log "two-node fault: $NODE + $PEER -> $TARGET_IP:$PORT (phase1=${PHASE1}s, then $NODE alone)"

nd_truth rule-6 node "$NODE"
for c in refused "closed port" "port $PORT" nd-r6s-c1 RST; do nd_truth rule-6 cause "$c"; done

SINCE=$(nd_now)
TOTAL=$((PHASE1 + WAIT_MIN * 60 + 120))
nd_fault_window "$TOTAL"
loop() { echo "
  for i in 1 2; do
    ( while [ \$(date +%s) -lt \$end ]; do
        curl -s -o /dev/null --connect-timeout 1 http://$TARGET_IP:$PORT/; sleep 0.25
      done ) &
  done; wait"; }
nd_bg nd-r6s-c1 "$TOTAL" "$(loop)"
nd_bg nd-r6s-c2 "$PHASE1" "$(loop)"

nd_log "phase 1: both nodes failing for ${PHASE1}s — rule-6 must stay silent"
sleep "$PHASE1"
nd_assert_silent "$SINCE" "rule-6" \
  || nd_die "guard violated: rule-6 reacted while two nodes were failing"

# Phase 2: c2's loop has expired on its own; only NODE keeps failing. PEER's
# rate[5m] needs up to 5 minutes to drain below the ratio, then for(2m).
nd_log "phase 2: $PEER stopped — waiting for rule-6 to fire on $NODE alone"
nd_wait "$SINCE" "rule-6" "$WAIT_MIN"
