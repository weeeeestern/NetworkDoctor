#!/usr/bin/env bash
# E2 호스트·우회 경로 DNS — CoreDNS를 거치지 않는 DNS를 호스트 수준에서 생성.
# 에이전트(물리 NIC tc 훅)는 이 질의를 세지만 coredns_*에는 전혀 안 잡힌다 →
# "두 소스가 서로의 사각지대를 커버"하는 상호보완 구조의 증명.
#
# 룰 발화가 목적이 아니라 대조 관측이 목적 — nd_wait 없음. 주입 후 Prometheus에서
# ebpf_dns_* vs coredns_* 를 같은 창으로 비교한다.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-300}

nd_preflight
NODE=$(nd_node)
nd_log "host-level DNS (CoreDNS 우회) on $NODE for ${DURATION}s — 대조 관측용"

# hostNetwork 파드: 노드 resolv.conf 경로 사용 → CoreDNS 안 거침.
# 외부 리졸버(8.8.8.8) 직접 질의 + 노드 기본 리졸버로 NXDOMAIN 유발, 둘 다.
nd_pod nd-hostdns "$NODE" host

SINCE=$(nd_now)
nd_fault_window "$DURATION"
# ⚠️ 외부 리졸버 직접 질의(@8.8.8.8/@1.1.1.1)만 사용 — 노드 resolv.conf나
# nslookup(인자 없는)을 쓰면 host 파드의 dnsPolicy=ClusterFirstWithHostNet
# 때문에 CoreDNS로 가버려 대조가 오염된다. 명시적 @server는 CoreDNS를 확실히 우회.
nd_bg nd-hostdns "$DURATION" "
  while [ \$(date +%s) -lt \$end ]; do
    dig @8.8.8.8 nope-\$(date +%s).nd-e2-test.invalid +time=2 +tries=1 >/dev/null 2>&1
    dig @1.1.1.1 bad-\$(date +%s).nd-e2-test.invalid +time=2 +tries=1 >/dev/null 2>&1
    dig @8.8.8.8 \$(date +%s).example.com +time=2 +tries=1 >/dev/null 2>&1
    sleep 0.4
  done"

nd_log "bypass DNS 생성 중 ${DURATION}s..."
sleep "$DURATION"

echo
echo "== E2 대조 관측 — Prometheus에서 같은 창으로 비교:"
echo "  [에이전트가 봄]  sum by(node)(increase(ebpf_dns_rcode_errors_total{node=\"$NODE\"}[${DURATION%.*}s]))"
echo "                   sum by(node)(increase(ebpf_dns_slow_total{node=\"$NODE\"}[${DURATION%.*}s]))"
echo "  [CoreDNS는 못 봄] increase(coredns_dns_responses_total{rcode=\"NXDOMAIN\"}[${DURATION%.*}s])  → 이 창의 호스트 질의는 미반영"
echo "  창: $SINCE ~ (현재)"
nd_log "주입 종료, 복원은 trap이 처리 (파드 삭제)"
