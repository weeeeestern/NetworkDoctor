# NetworkDoctor

> eBPF와 Prometheus로 Kubernetes 네트워크 장애 신호를 수집하고 원인 후보를 추론하는 Incident 진단 도구

NetworkDoctor는 Kubernetes 클러스터에서 발생하는 네트워크 장애를 더 빠르게 관찰하고 진단하기 위한 졸업 프로젝트입니다.

노드 단위의 네트워크 신호를 수집하고, Prometheus 시계열 데이터를 기반으로 이상 징후를 탐지한 뒤, 규칙 기반 점수화를 통해 가능한 원인 후보와 근거 메트릭, 권장 조치를 Incident 형태로 제공합니다.

## Background

프로젝트를 시작하기 전, 팀원들이 공통 배경지식을 맞추기 위해 Kubernetes/Linux 네트워크, 커널 메트릭, 네트워크 토폴로지를 중심으로 사전 스터디를 진행했습니다.

사전 스터디 자료는 [prestudy](./prestudy) 폴더에 정리되어 있습니다.

## Project Goal

NetworkDoctor의 목표는 장애 상황에서 다음 흐름을 하나의 진단 경험으로 연결하는 것입니다.

1. 클러스터와 서비스/Pod 구조를 설계한다.
2. 네트워크 장애 시나리오를 구상하고 주입한다.
3. 장애 발생 시 나타나는 증상과 메트릭 변화를 관찰한다.
4. 수집할 신호와 메트릭을 정의한다.
5. 메트릭 조합을 기반으로 원인 후보를 추론한다.
6. Incident UI, Grafana/Kiali 대시보드, Markdown/PDF 리포트로 결과를 보여준다.

## Architecture

![alt text](networkdoctor-flow.png)

```text
Kubernetes Cluster
├─ Node Agent DaemonSet
│  ├─ eBPF program
│  ├─ kernel /proc, /sys, netlink metrics
│  ├─ userspace aggregator
│  └─ Prometheus /metrics endpoint
│
├─ Prometheus
│  └─ scrape node and service metrics
│
├─ Diagnosis Backend
│  ├─ anomaly detector
│  ├─ rule engine
│  ├─ root cause candidate scorer
│  ├─ incident data model
│  └─ report generator
│
└─ Grafana / Kiali / Incident UI / Helm
   ├─ dashboards
   ├─ incident visualization
   ├─ demo environment
   └─ installable deployment package
```

## Core Components

### 1. Node Agent / eBPF / Metrics Collection

노드 단위 네트워크 신호 수집을 담당합니다. eBPF 프로그램과 userspace aggregator를 구현하고, DaemonSet 기반 Node Agent가 Prometheus에서 scrape 가능한 `/metrics` 엔드포인트를 제공합니다.

담당: 동욱, 은서

- eBPF attach 지점 선정
- BPF map / ring buffer 설계
- userspace aggregator 구현
- Prometheus metrics export
- DaemonSet 배포 구성
- eBPF로 직접 수집할 신호와 기존 exporter/커널 인터페이스로 대체할 신호 구분

### 2. Diagnosis Backend / Rule Engine / Report

Prometheus 시계열 데이터를 기반으로 이상 징후를 감지하고, 규칙 기반 점수화를 통해 원인 후보를 추론합니다. 생성된 진단 결과는 Incident 데이터 모델로 저장하고 Markdown/PDF 리포트로 출력합니다.

담당: 서영, 은서

- Prometheus API에서 최근 시계열 조회
- threshold, window, baseline 대비 증가율 기반 이상 징후 탐지
- weighted score 기반 root cause 후보 점수화
- Top-N 원인 후보 출력
- evidence metrics 연결
- recommended actions 생성
- Markdown/PDF incident report 생성

### 3. Prometheus / Grafana / Kiali / UI / Helm / Demo

NetworkDoctor를 설치 가능하고 시각적으로 확인할 수 있는 형태로 구성합니다. Prometheus/Grafana/Kiali 연동, Incident UI, Helm chart, 장애 주입 및 데모 환경을 담당합니다.

담당: 서영, 동욱

- Prometheus scrape 연결
- Grafana/Kiali dashboard 구성
- Incident UI 구현
- Helm chart / 설치 스크립트 정리
- 장애 주입 시나리오 작성
- 데모 및 검증 플로우 문서화

## Incident Data Model

Diagnosis Backend는 장애 상황을 Incident 단위로 저장합니다.

```text
incident
├─ incident_id
├─ created_at
├─ severity
├─ symptom_summary
├─ affected_services
├─ affected_nodes
├─ root_cause_candidates
├─ evidence_metrics
├─ recommended_actions
└─ recovery_status
```

## Metrics and Rules

초기 진단 대상은 다음과 같은 네트워크 신호와 메트릭 조합을 중심으로 설계합니다.

| Signal | Example Metrics | Diagnosis Candidate |
| --- | --- | --- |
| TCP retransmit 증가 | retransmit rate, RTT p95/p99 | 혼잡 또는 패킷 손실 가능성 |
| softnet drop 증가 | softnet dropped, node CPU, IRQ pressure | 노드 수신 처리 병목 가능성 |
| DNS 지연/실패 증가 | DNS p99 latency, SERVFAIL count | CoreDNS 병목 또는 DNS 장애 가능성 |
| conntrack 사용률 증가 | conntrack usage, dropped/new failures | conntrack table 포화 가능성 |

## Expected Deliverables

- Node Agent 동작 코드
- Prometheus metrics 명세서
- Diagnosis Backend API 또는 JSON 출력
- Incident schema
- rule table
- 샘플 metrics 출력
- 실제 incident report 예시
- Grafana/Kiali dashboard JSON / 스크린샷
- Incident UI 화면
- Helm chart
- 클러스터 배포 가이드
- 장애 주입 및 실험 재현 절차

## Repository Structure

```text
.
├─ .github/workflows/        # agent-ci, agent-image, backend-image, deploy-ci
├─ bpf/
│  └─ networkdoctor.bpf.c    # kernel-side eBPF collector
├─ build/
│  ├─ agent/Dockerfile
│  └─ backend/Dockerfile
├─ cmd/
│  ├─ networkdoctor-agent/   # eBPF loader + Prometheus exporter (Linux only)
│  └─ networkdoctor-backend/ # Alertmanager webhook -> incident JSON
├─ internal/
│  ├─ app/ bpf/ config/ metrics/ model/ worker/   # agent
│  └─ backend/ api/ alertmanager/ config/ incident/ # backend
├─ deploy/
│  ├─ helm/networkdoctor/    # DaemonSet, Service, ServiceMonitor, PrometheusRule/ConfigMap, backend
│  ├─ alertmanager/          # route/receiver examples
│  └─ prometheus/rules/      # rule file notes
├─ docs/deploy-onprem.md     # on-prem MVP install / verification / rollback
├─ prestudy/
├─ go.mod
├─ go.sum
└─ README.md
```

MVP 단계의 책임 분리는 다음과 같습니다. 탐지(firing/resolved)는 Prometheus 룰이, 라우팅·그룹핑·재전송은 Alertmanager가, 인시던트 기록·근거 수집·HolmesGPT 호출·리포트는 backend가 맡습니다. 자세한 설치 절차는 [docs/deploy-onprem.md](./docs/deploy-onprem.md)를 참고하세요.

## Repository Description

```text
eBPF와 Prometheus로 Kubernetes 네트워크 장애 신호를 수집하고 원인 후보를 추론하는 Incident 진단 도구
```
