#!/usr/bin/env bash
# Rule 1 · network-congestion
# Application latency rises together with TCP retransmits on the same node.
#
# A client pod on a Cat Shop node gets 30% egress packet loss (tc netem on its own
# eth0) and calls Cat Shop /delay/1 from 16 parallel loops. The losses cause
# retransmits on ND_NODE and push Cat Shop p99 latency above 500ms.
# Needs the networkdoctor-demo chart.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-18}
LOSS=${ND_LOSS:-30%}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-1"
# Rule 1 joins app latency and retransmits on the same node, and the latency
# is labelled with the Cat Shop pod's node. So the client shares a node with a
# Cat Shop pod. ND_RULE1_MODE=cross puts it on a node WITHOUT Cat Shop instead,
# which must fire the cross-node variant (severity info) and not Rule 1.
MODE=${ND_RULE1_MODE:-same}
CATSHOP_NODES=$(kubectl -n "$ND_DEMO_NS" get pod \
  -l app.kubernetes.io/name=catshop,app.kubernetes.io/component=app \
  -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u)
[[ -n "$CATSHOP_NODES" ]] || nd_die "no Cat Shop pod found in $ND_DEMO_NS"
case "$MODE" in
  same)  NODE=${ND_NODE:-$(head -1 <<<"$CATSHOP_NODES")} ;;
  cross) NODE=${ND_NODE:-$(nd_workers | grep -vxF "$CATSHOP_NODES" | head -1)} ;;
  *)     nd_die "ND_RULE1_MODE must be same or cross" ;;
esac
[[ -n "$NODE" ]] || nd_die "no suitable node for mode=$MODE (set ND_NODE)"
nd_log "mode=$MODE node=$NODE loss=$LOSS target=$ND_CATSHOP_URL/delay/1 duration=${DURATION}s"

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
