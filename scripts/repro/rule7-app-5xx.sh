#!/usr/bin/env bash
# Rule 7 · application-induced-network-bottleneck
# The application returns 5xx while the network stays healthy.
#
# A client pod sends ~10 req/s to Cat Shop /status/500. The Cat Shop loadgen
# keeps sending its normal ~10 req/s of 200s, so the 5xx ratio sits near 50%
# and no node shows TCP retransmits. Needs the networkdoctor-demo chart.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-14}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-7"
NODE=$(nd_node)
nd_log "client node=$NODE target=$ND_CATSHOP_URL/status/500 duration=${DURATION}s"

nd_pod nd-r7-client "$NODE" pod
SINCE=$(nd_now)
nd_fault_window "$DURATION"
nd_bg nd-r7-client "$DURATION" "
  for i in 1 2 3 4; do
    ( while [ \$(date +%s) -lt \$end ]; do
        curl -s -o /dev/null --max-time 3 $ND_CATSHOP_URL/status/500; sleep 0.35
      done ) &
  done; wait"

nd_wait "$SINCE" "rule-7" "$WAIT_MIN"
