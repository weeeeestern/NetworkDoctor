#!/usr/bin/env bash
# T5 배포 직후 에러 — catshop을 반복 rollout restart 하면서 부하 유지. 새 파드가
# 준비되는 동안 5xx/연결거부가 지속적으로 발생한다. 증상은 네트워크 장애와 같지만
# 원인은 "방금 배포"다.
#   정답: root_cause=롤아웃된 파드/배포, 네트워크 신호 정상. 배포 이벤트 인용.
#   오답: 네트워크·정책 방향 추측.
# rollout 1회는 readiness 갭이 짧아 rule-7(for:3m)이 안 울 수 있으므로 반복 롤링으로
# 5xx를 지속시킨다. 클라이언트가 catshop에 꾸준히 요청해 5xx 비율을 올린다.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-16}
DEPLOY=${ND_CATSHOP_DEPLOY:-catshop}
ROLL_EVERY=${ND_ROLL_EVERY:-60}   # 초마다 재롤링

nd_preflight
nd_backend_connect
nd_require_quiet "rule-7"
NODE=$(nd_node)
nd_log "T5: $DEPLOY 반복 rollout(${ROLL_EVERY}s마다) + 부하 ${DURATION}s — 원인은 배포(네트워크 아님)"

# 부하 클라이언트 (readiness 갭 동안 5xx/거부를 관측하게)
nd_pod nd-t5-client "$NODE" pod

nd_truth rule-7 service catshop
nd_truth rule-7 cause "rollout"
nd_truth rule-7 cause "deploy"
nd_truth rule-7 cause "restart"

SINCE=$(nd_now)
nd_fault_window "$DURATION"
# 부하: catshop 루트로 꾸준히 (readiness 안 된 파드로 가면 5xx/거부)
nd_bg nd-t5-client "$DURATION" "
  for i in 1 2 3 4 5 6 7 8; do
    ( while [ \$(date +%s) -lt \$end ]; do curl -s -o /dev/null --max-time 3 $ND_CATSHOP_URL/; sleep 0.1; done ) &
  done; wait"

# 반복 롤링을 백그라운드로 (fault window 동안)
( END=$(( $(date +%s) + DURATION ))
  while [ "$(date +%s)" -lt "$END" ]; do
    kubectl -n "$ND_DEMO_NS" rollout restart deploy/"$DEPLOY" >/dev/null 2>&1
    nd_log "rollout restart $DEPLOY"
    sleep "$ROLL_EVERY"
  done ) &
ROLL_PID=$!
nd_on_exit "kill $ROLL_PID 2>/dev/null; kubectl -n $ND_DEMO_NS rollout status deploy/$DEPLOY --timeout=120s >/dev/null 2>&1"

nd_wait "$SINCE" "rule-7" "$WAIT_MIN"
echo
nd_log "★ 채점(T5): 롤아웃/배포 이벤트를 원인으로 짚었는가, 네트워크 탓으로 돌렸는가"
nd_log "★ 복원: 롤링 중단 + 최종 rollout 안정화까지 대기(trap)"
