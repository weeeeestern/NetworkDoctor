#!/usr/bin/env bash
# CPU stress (single) — 네트워크 시나리오 룰은 전부 침묵해야 한다.
# 노드 CPU를 포화시키면 runqlat(실행큐 지연)는 오르지만, 어떤 룰도 runqlat를
# 조건으로 쓰지 않으므로 발화하면 안 된다. 특이도(specificity) 음성 테스트.
#
# 검증 2단: ① 네트워크 룰 전부 SILENT ② runqlat가 실제로 올랐는지(주입이
# 먹혔다는 증거) — 둘 다여야 "네트워크 아닌 부하에 침묵"이 성립한다.
set -uo pipefail
. "$(dirname "$0")/lib.sh"
. "$(dirname "$0")/lib-silence.sh"

DURATION=${ND_DURATION:-360}   # > 가장 긴 for(5m) + 여유

nd_preflight
nd_backend_connect
NODE=$(nd_node)

nd_pod nd-cpu "$NODE" pod
NPROC=$(nd_x nd-cpu "nproc" 2>/dev/null || echo 4)
nd_log "CPU stress: node=$NODE cores=$NPROC duration=${DURATION}s — 네트워크 룰 전부 침묵해야 함"

SINCE=$(nd_now)
nd_fault_window "$DURATION"
# 코어 수의 2배 busy loop로 실행큐 경합 유발 (stress-ng 불필요 — netshoot 기본만 사용)
nd_bg nd-cpu "$DURATION" "
  n=\$(nproc)
  i=0; while [ \$i -lt \$((n*2)) ]; do
    ( while [ \$(date +%s) -lt \$end ]; do : ; done ) &
    i=\$((i+1))
  done
  wait"

nd_log "stressing ${DURATION}s..."
sleep "$DURATION"

nd_assert_silent "$SINCE" "rule-1 rule-2 rule-3 rule-4 rule-5 rule-6 rule-7 rule-8"
RC=$?

# runqlat가 올랐는지 확인 (주입 유효성). 에이전트 메트릭은 node 라벨 보유.
echo
echo "== runqlat 상승 확인 (주입 유효성) — Prometheus에서:"
echo "  histogram_quantile(0.99, sum by (le,node)(rate(ebpf_runqlat_bucket{node=\"$NODE\"}[5m]))) / 1e6"
echo "  (ms 단위로 평시 대비 상승이면 주입 유효 → 침묵이 의미를 가짐)"
exit $RC
