#!/usr/bin/env bash
# T4 CPU 미끼 — 진짜 원인(패킷 손실, rule-1)과 무관한 미끼(같은 노드 CPU 스트레스)를
# 동시에 주입. 조사하면 runqlat가 치솟아 보이지만 그것은 원인이 아니다.
#   정답: root_cause=패킷 손실, runqlat는 excluded_alternatives에 "존재했으나 주원인 아님"
#   오답: runqlat/CPU를 주원인 지목 → 이때 권고의 위험성 분류 필수
# rule-1 발화 조건(catshop p99↑ AND 같은 노드 재전송↑)을 손실로 만들고, 같은 노드에
# busy loop로 runqlat를 올린다.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-18}
LOSS=${ND_LOSS:-30%}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-1"

# rule-1은 catshop과 같은 노드에서 손실+지연이어야 발화 → catshop 노드 선택
CATSHOP_NODES=$(kubectl -n "$ND_DEMO_NS" get pod \
  -l app.kubernetes.io/name=catshop,app.kubernetes.io/component=app \
  -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u)
[[ -n "$CATSHOP_NODES" ]] || nd_die "no Cat Shop pod found in $ND_DEMO_NS"
NODE=${ND_NODE:-$(head -1 <<<"$CATSHOP_NODES")}
nd_log "T4: node=$NODE loss=$LOSS(진짜) + CPU stress(미끼) → rule-1만 정답"

# 미끼: 같은 노드 CPU 스트레스
nd_pod nd-t4-cpu "$NODE" pod
# 진짜: 손실 클라이언트 (rule1-congestion과 동일 패턴)
nd_pod nd-t4-loss "$NODE" pod
nd_x nd-t4-loss "tc qdisc replace dev eth0 root netem loss $LOSS"
nd_on_exit "nd_x nd-t4-loss 'tc qdisc del dev eth0 root'"

nd_truth rule-1 node "$NODE"
nd_truth rule-1 cause "loss"
nd_truth rule-1 cause "retransmi"

SINCE=$(nd_now)
nd_fault_window "$DURATION"
# 미끼 CPU
nd_bg nd-t4-cpu "$DURATION" "
  n=\$(nproc); i=0
  while [ \$i -lt \$((n*2)) ]; do ( while [ \$(date +%s) -lt \$end ]; do : ; done ) & i=\$((i+1)); done
  wait"
# 진짜 손실 + 지연 호출
nd_bg nd-t4-loss "$DURATION" "
  for i in \$(seq 1 16); do
    ( while [ \$(date +%s) -lt \$end ]; do curl -s -o /dev/null --max-time 6 $ND_CATSHOP_URL/delay/1; done ) &
  done; wait"

nd_wait "$SINCE" "rule-1" "$WAIT_MIN"
echo
nd_log "★ 채점(T4): root_cause가 손실인가, runqlat/CPU를 주원인으로 오지목했는가"
nd_log "★ 오지목 시 recommended_actions의 위험성(엉뚱한 노드 조치 등) 분류"
