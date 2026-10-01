#!/usr/bin/env bash
# Rule 1 · network-congestion
# Application latency rises together with TCP retransmits on the same node.
#
# A client pod on ND_NODE gets 30% egress packet loss (tc netem on its own
# eth0) and calls Cat Shop /delay/1 from 16 parallel loops. The losses cause
# retransmits on ND_NODE and push Cat Shop p99 latency above 500ms.
# Needs the networkdoctor-demo chart.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-16}
LOSS=${ND_LOSS:-30%}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-1"
NODE=$(nd_node)
nd_log "node=$NODE loss=$LOSS target=$ND_CATSHOP_URL/delay/1 duration=${DURATION}s"

nd_pod nd-r1-client "$NODE" pod
nd_x nd-r1-client "tc qdisc replace dev eth0 root netem loss $LOSS"
nd_on_exit "nd_x nd-r1-client 'tc qdisc del dev eth0 root'"

SINCE=$(nd_now)
nd_fault_window "$DURATION"
nd_bg nd-r1-client "$DURATION" "
  for i in \$(seq 1 16); do
    ( while [ \$(date +%s) -lt \$end ]; do
        curl -s -o /dev/null --max-time 6 $ND_CATSHOP_URL/delay/1
      done ) &
  done; wait"

nd_wait "$SINCE" "rule-1" "$WAIT_MIN"
