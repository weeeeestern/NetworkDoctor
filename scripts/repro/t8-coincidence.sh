#!/usr/bin/env bash
# T8 우연 동거 — 서로 무관한 두 장애를 같은 노드·10분 안에 주입한다.
#   A(rule-8): UDP deny-all ingress 정책 — 드랍은 target이 있는 노드에서 집계
#   B(rule-6): 닫힌 포트로 TCP 연결 — 실패는 client가 있는 노드에서 집계
# UDP를 쓰는 게 핵심: 정책거부가 연결 실패 카운터로 새지 않아 rule-6과 섞이지 않는다.
# 정답은 "별개" — 정책 제거 시 rule-8만 사라지고 rule-6은 유지(반사실 테스트).
#
# ⚠️ 노드 배치 가정: rule-8 target과 rule-6 client를 둘 다 NODE에 둬서 두 발화가
# 같은 노드에 surface → 그룹핑 유도. 첫 런에서 correlation_id 동일 여부를 반드시 확인.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-16}
PORT=${ND_CLOSED_PORT:-9}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-6 rule-8"
NODE=$(nd_node)
PEER=$(nd_peer_node "$NODE")

# A — rule-8: target을 NODE에 두어 ingress-deny 드랍이 NODE에서 집계되게.
nd_pod nd-t8-r8target "$NODE" pod
R8_IP=$(kubectl -n "$ND_DEMO_NS" get pod nd-t8-r8target -o jsonpath='{.status.podIP}')
nd_pod nd-t8-r8client "$PEER" pod
nd_on_exit "kubectl -n $ND_DEMO_NS delete networkpolicy nd-t8-deny --ignore-not-found"
kubectl apply -f - >/dev/null <<EOF || nd_die "could not create NetworkPolicy"
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nd-t8-deny
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true"}
spec:
  podSelector: {matchLabels: {app: nd-t8-r8target}}
  policyTypes: [Ingress]
  ingress: []
EOF

# B — rule-6: client를 NODE에, 닫힌 포트 target을 PEER에.
nd_pod nd-t8-r6target "$PEER" pod
R6_IP=$(kubectl -n "$ND_DEMO_NS" get pod nd-t8-r6target -o jsonpath='{.status.podIP}')
nd_pod nd-t8-r6client "$NODE" pod

nd_log "T8: rule-8(UDP deny, target on $NODE) + rule-6(closed port :$PORT on $PEER, client on $NODE) — 별개 원인"
nd_truth rule-8 policy nd-t8-deny
nd_truth rule-8 cause "networkpolicy"
nd_truth rule-6 cause "closed port"
nd_truth rule-6 cause "refused"

SINCE=$(nd_now)
nd_fault_window "$DURATION"
# rule-8: UDP를 정책이 막는 target으로 (연결 실패 카운터와 무관)
nd_bg nd-t8-r8client "$DURATION" "
  while [ \$(date +%s) -lt \$end ]; do echo nd-t8 | nc -u -w1 $R8_IP 8080; sleep 0.2; done"
# rule-6: 닫힌 포트로 TCP (refused → connect_failed)
nd_bg nd-t8-r6client "$DURATION" "
  for i in 1 2; do
    ( while [ \$(date +%s) -lt \$end ]; do curl -s -o /dev/null --connect-timeout 1 http://$R6_IP:$PORT/; sleep 0.25; done ) &
  done; wait"

nd_wait "$SINCE" "rule-6 rule-8" "$WAIT_MIN"
echo
nd_log "★ 채점 전 확인: 두 인시던트의 correlation_id가 같은가(그룹됨)? 다르면 T8 전제 미성립 → 노드 배치 조정"
nd_log "★ 반사실: 정책 제거 시 rule-8만 소멸해야 별개 (T8 정답)"
