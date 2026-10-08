#!/usr/bin/env bash
# Rule 8 · networkpolicy-misconfiguration, several situations.
#
#   ND_RULE8_MODE=ingress-udp  deny-all ingress on a target; client sends UDP   -> Rule 8 fires
#   ND_RULE8_MODE=ingress-tcp  same, but the client opens TCP connections      -> Rule 8 fires (TCP
#                              timeouts also count as connect failures on the
#                              client node, so Rule 6 may fire as well)
#   ND_RULE8_MODE=egress       deny all egress from the client except DNS      -> Rule 8 fires on
#                              the client's node (drop on the sending side)
#   ND_RULE8_MODE=allow        ingress policy that allows the client           -> Rule 8 must NOT fire
#
# Needs Cilium with Hubble "drop" metrics. Client and target run on different
# nodes so the drop node is unambiguous.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

MODE=${ND_RULE8_MODE:-ingress-udp}
DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-14}
case "$MODE" in ingress-udp | ingress-tcp | egress | allow) ;; *) nd_die "ND_RULE8_MODE must be ingress-udp, ingress-tcp, egress or allow" ;; esac

nd_preflight
nd_backend_connect
nd_require_quiet "rule-8"
NODE=$(nd_node)
PEER=$(nd_peer_node "$NODE")

nd_pod nd-r8v-target "$PEER" pod
nd_pod nd-r8v-client "$NODE" pod
TARGET_IP=$(kubectl -n "$ND_DEMO_NS" get pod nd-r8v-target -o jsonpath='{.status.podIP}')
nd_on_exit "kubectl -n $ND_DEMO_NS delete networkpolicy nd-r8v --ignore-not-found"

case "$MODE" in
  ingress-udp | ingress-tcp)
    POLICY_SPEC='podSelector: {matchLabels: {app: nd-r8v-target}}
  policyTypes: [Ingress]
  ingress: []'
    DROP_NODE=$PEER ;;
  egress)
    POLICY_SPEC='podSelector: {matchLabels: {app: nd-r8v-client}}
  policyTypes: [Egress]
  egress:
    - ports: [{port: 53, protocol: UDP}, {port: 53, protocol: TCP}]'
    DROP_NODE=$NODE ;;
  allow)
    POLICY_SPEC='podSelector: {matchLabels: {app: nd-r8v-target}}
  policyTypes: [Ingress]
  ingress:
    - from: [{podSelector: {matchLabels: {app: nd-r8v-client}}}]'
    DROP_NODE="" ;;
esac
kubectl apply -f - >/dev/null <<EOF || nd_die "could not create NetworkPolicy"
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nd-r8v
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true"}
spec:
  $POLICY_SPEC
EOF
nd_log "mode=$MODE client=$NODE target=$TARGET_IP on $PEER, expected drop node=${DROP_NODE:-none}"

if [[ -n "$DROP_NODE" ]]; then
  nd_truth rule-8 policy nd-r8v
  nd_truth rule-8 target nd-r8v-target
  nd_truth rule-8 target nd-r8v-client
fi

SINCE=$(nd_now)
nd_fault_window "$DURATION"
if [[ "$MODE" == ingress-tcp ]]; then
  nd_bg nd-r8v-client "$DURATION" "
    while [ \$(date +%s) -lt \$end ]; do
      curl -s -o /dev/null --connect-timeout 2 http://$TARGET_IP:8080/; sleep 0.2
    done"
else
  nd_bg nd-r8v-client "$DURATION" "
    while [ \$(date +%s) -lt \$end ]; do
      echo nd-rule8 | nc -u -w1 $TARGET_IP 8080; sleep 0.2
    done"
fi

if [[ "$MODE" == allow ]]; then
  # Negative case: wait through the rule's pending period, then require that
  # no Rule 8 incident appeared.
  nd_log "allow mode: waiting ${WAIT_MIN}m and expecting no rule-8 incident"
  sleep $((WAIT_MIN * 60))
  echo
  echo "== NetworkDoctor repro result (since $SINCE)"
  if nd_status "$SINCE" "rule-8" >/dev/null 2>&1 || nd_status "$SINCE" "rule-8" 2>/dev/null | grep -qv 'no incident yet'; then
    echo "  rule-8  FAIL   a Rule 8 incident appeared although the policy allows the traffic"
    exit 1
  fi
  echo "  rule-8  PASS   no Rule 8 incident (policy allows the traffic)"
  exit 0
fi

nd_wait "$SINCE" "rule-8" "$WAIT_MIN"
