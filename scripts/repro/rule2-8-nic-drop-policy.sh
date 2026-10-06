#!/usr/bin/env bash
# Rule 2 · kernel-network-bottleneck
# Rule 8 · networkpolicy-misconfiguration
#
# Rule 2: a hostNetwork pod on ND_PEER_NODE sends ~50 Ethernet frames/s with
#         an unused EtherType (0x88b5) to ND_NODE's NIC. The kernel has no
#         handler for them and counts them in the NIC's rx_dropped.
# Rule 8: a target pod gets a deny-all-ingress NetworkPolicy while a client
#         keeps sending it UDP datagrams, so Cilium/Hubble report
#         POLICY_DENIED drops. UDP, not TCP: denied TCP connects would also
#         count as connect failures on this node and fire Rule 6, which then
#         groups with Rule 2.
#
# Rule 8 needs a CNI that enforces NetworkPolicy and exports Hubble drop
# metrics. The two rules are on different nodes, so they are not correlated.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-540}
WAIT_MIN=${ND_WAIT_MIN:-16}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-2 rule-8"
NODE=$(nd_node)
PEER=$(nd_peer_node "$NODE")
IFACE=$(nd_iface)
[[ -n "$IFACE" ]] || nd_die "could not read the agent -iface (set ND_IFACE)"

# Rule 2: read the receiver's MAC on the node itself, then send from the peer.
nd_pod nd-r2-rx "$NODE" host
DST_MAC=$(nd_x nd-r2-rx "cat /sys/class/net/$IFACE/address")
[[ "$DST_MAC" =~ ^([0-9a-f]{2}:){5}[0-9a-f]{2}$ ]] || nd_die "bad MAC '$DST_MAC' for $IFACE on $NODE"
nd_pod nd-r2-sender "$PEER" host
nd_x nd-r2-sender "test -e /sys/class/net/$IFACE" || nd_die "$PEER has no $IFACE (set ND_IFACE)"
nd_log "rule-2: $PEER -> $NODE $IFACE ($DST_MAC), 50 frames/s"

# Rule 8: an isolated target, a client and a policy that denies all ingress.
nd_pod nd-r8-target "$PEER" pod
nd_pod nd-r8-client "$NODE" pod
TARGET_IP=$(kubectl -n "$ND_DEMO_NS" get pod nd-r8-target -o jsonpath='{.status.podIP}')
nd_on_exit "kubectl -n $ND_DEMO_NS delete networkpolicy nd-repro-deny --ignore-not-found"
kubectl apply -f - >/dev/null <<EOF || nd_die "could not create NetworkPolicy"
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nd-repro-deny
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true"}
spec:
  podSelector: {matchLabels: {app: nd-r8-target}}
  policyTypes: [Ingress]
  ingress: []
EOF
nd_log "rule-8: deny-all ingress on nd-r8-target ($TARGET_IP)"

nd_truth rule-2 node "$NODE"
nd_truth rule-2 iface "$IFACE"
nd_truth rule-8 policy nd-repro-deny
nd_truth rule-8 target nd-r8-target
SINCE=$(nd_now)
nd_fault_window "$DURATION"
nd_bg nd-r2-sender "$DURATION" "python3 -c \"
import socket, time
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW); s.bind(('$IFACE', 0))
src = open('/sys/class/net/$IFACE/address').read().strip().replace(':', '')
frame = bytes.fromhex('${DST_MAC//:/}') + bytes.fromhex(src) + b'\\\\x88\\\\xb5' + b'networkdoctor-rule2'.ljust(46, b'\\\\x00')
stop = time.time() + $DURATION
while time.time() < stop:
    s.send(frame); time.sleep(0.02)
\""
nd_bg nd-r8-client "$DURATION" "
  while [ \$(date +%s) -lt \$end ]; do
    echo nd-rule8 | nc -u -w1 $TARGET_IP 8080; sleep 0.2
  done"

nd_wait "$SINCE" "rule-2 rule-8" "$WAIT_MIN"
