#!/usr/bin/env bash
# T6 메모리 부족 재시작 — 별도 OOM 워크로드를 띄워 OOMKill 재시작을 유발. 그 동안
# 클라이언트가 그 워크로드로 요청하면 연결 리셋/거부가 발생한다. 증상은 네트워크
# 장애(연결 끊김)와 같지만 원인은 메모리다.
#   정답: root_cause=OOMKilled 재시작, 재시작 횟수/파드 이벤트 근거.
#   오답: conntrack·정책·노드 장애 방향 추측.
#
# ⚠️ catshop(공유 데모 앱)의 limit를 건드리지 않는다 — 전용 OOM 파드를 만들어
# 격리. 복원은 파드 삭제뿐(trap)이라 공유 자원 영향 없음.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-420}
WAIT_MIN=${ND_WAIT_MIN:-16}
MEM=${ND_OOM_MEM:-32Mi}        # 빠듯한 한도

nd_preflight
nd_backend_connect
nd_require_quiet "rule-6"       # 연결 실패로 rule-6이 울 가능성 — 그 전제로 quiet 확인
NODE=$(nd_node)
nd_log "T6: OOM 워크로드(limit $MEM) on $NODE — 원인은 메모리(네트워크 아님)"

# OOM 타깃: 메모리 한도 빠듯한 서버 파드. 요청 받으면 메모리 할당해 OOMKill 유도.
# restartPolicy Always가 아니라 Deployment가 필요 → 간단히 Deployment 생성.
nd_on_exit "kubectl -n $ND_DEMO_NS delete deploy nd-t6-oom --ignore-not-found --wait=false"
kubectl apply -f - >/dev/null <<EOF || nd_die "could not create OOM deploy"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nd-t6-oom
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true"}
spec:
  replicas: 1
  selector: {matchLabels: {app: nd-t6-oom}}
  template:
    metadata: {labels: {app: nd-t6-oom, $ND_LABEL_KEY: "true"}}
    spec:
      nodeName: $NODE
      containers:
      - name: s
        image: $ND_TOOL_IMAGE
        resources: {limits: {memory: "$MEM"}}
        # 포트 8080에서 nc 리스너; 접속 시마다 메모리를 조금씩 먹다 OOMKill
        command: ["sh","-c"]
        args:
        - |
          while true; do
            nc -lk -p 8080 -e sh -c 'head -c 20m /dev/zero | tail' 2>/dev/null || \
            { timeout 3 nc -l -p 8080 >/dev/null 2>&1; dd if=/dev/zero bs=1M count=20 2>/dev/null | cat >/dev/null; }
          done
EOF
kubectl -n "$ND_DEMO_NS" rollout status deploy/nd-t6-oom --timeout=120s >/dev/null 2>&1 || true
OOM_IP=$(kubectl -n "$ND_DEMO_NS" get pod -l app=nd-t6-oom -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)

nd_pod nd-t6-client "$NODE" pod
nd_truth rule-6 cause "oom"
nd_truth rule-6 cause "memory"
nd_truth rule-6 cause "restart"
nd_truth rule-6 cause "nd-t6-oom"

SINCE=$(nd_now)
nd_fault_window "$DURATION"
# 클라이언트: OOM 파드로 반복 접속 → 재시작 중 연결 리셋/거부
nd_bg nd-t6-client "$DURATION" "
  for i in 1 2 3 4; do
    ( while [ \$(date +%s) -lt \$end ]; do
        curl -s -o /dev/null --connect-timeout 1 --max-time 2 http://${OOM_IP:-127.0.0.1}:8080/ 2>/dev/null; sleep 0.25
      done ) &
  done; wait"

nd_wait "$SINCE" "rule-6" "$WAIT_MIN"
echo
nd_log "★ 채점(T6): OOMKilled/재시작을 원인으로 파고들었는가, 네트워크 탓으로 돌렸는가"
nd_log "★ 참고: kubectl -n $ND_DEMO_NS get pod -l app=nd-t6-oom (재시작 횟수 확인)"
