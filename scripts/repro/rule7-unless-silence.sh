#!/usr/bin/env bash
# Rule 7 guard · unless
# 5xx load together with real packet loss must keep Rule 7 silent: its expr
# drops the alert while any node retransmits above 0.2/s, because the errors
# might be the network's fault and Rule 1 owns that path. Phase 2 stops the
# loss client; once the retransmit rate drains, the same 5xx load must fire
# Rule 7 — proving the phase-1 silence came from the unless, not from a
# missing 5xx signal.
#
# The loss client calls plain / (not /delay), so the server-side latency
# histogram stays low and Rule 1 must stay silent too: phase 1 is expected to
# be fully silent on both rules. The smoke retransmit-burst alert may fire
# during phase 1; it is not part of this assertion.
set -uo pipefail
. "$(dirname "$0")/lib.sh"
. "$(dirname "$0")/lib-silence.sh"

PHASE1=${ND_PHASE1_SEC:-420}   # 5xx + loss together; > rate ramp + for(3m)
WAIT_MIN=${ND_WAIT_MIN:-14}    # phase 2: retransmit rate[5m] decay + for(3m)
LOSS=${ND_LOSS:-30}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-7 rule-1"
NODE=$(nd_node)
nd_log "unless test on $NODE: /status/500 at ~10rps + ${LOSS}% egress loss client (phase1=${PHASE1}s)"

nd_pod nd-r7s-5xx "$NODE" pod
nd_pod nd-r7s-loss "$NODE" pod
nd_x nd-r7s-loss "tc qdisc add dev eth0 root netem loss ${LOSS}%" \
  || nd_die "could not add netem on the loss client"

nd_truth rule-7 service catshop
SINCE=$(nd_now)
TOTAL=$((PHASE1 + WAIT_MIN * 60 + 120))
nd_fault_window "$TOTAL"
# 5xx runs through both phases; the loss loop expires with phase 1.
nd_bg nd-r7s-5xx "$TOTAL" "
  for i in 1 2 3 4; do
    ( while [ \$(date +%s) -lt \$end ]; do
        curl -s -o /dev/null --max-time 3 $ND_CATSHOP_URL/status/500; sleep 0.35
      done ) &
  done; wait"
nd_bg nd-r7s-loss "$PHASE1" "
  for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
    ( while [ \$(date +%s) -lt \$end ]; do
        curl -s -o /dev/null --max-time 2 $ND_CATSHOP_URL/; sleep 0.2
      done ) &
  done; wait"

nd_log "phase 1: 5xx + retransmits for ${PHASE1}s — rule-7 (and rule-1) must stay silent"
sleep "$PHASE1"
nd_assert_silent "$SINCE" "rule-7 rule-1" \
  || nd_die "guard violated: a rule reacted while the unless condition was active"

# Phase 2: the loss loop has expired; drop the netem so nothing retransmits.
nd_x nd-r7s-loss "tc qdisc del dev eth0 root" >/dev/null 2>&1
nd_log "phase 2: loss stopped — waiting for rule-7 to fire on the 5xx signal alone"
nd_wait "$SINCE" "rule-7" "$WAIT_MIN"
