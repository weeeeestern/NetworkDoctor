# NetworkDoctor 설치부터 Slack·Grafana·UI·PDF 확인까지

설치 담당자가 고객의 Kubernetes에 프로그램을 설치하고, 팀원에게 접속 주소를 전달하는 절차입니다. **에이전트·백엔드·UI·Prometheus·Grafana는 고객 환경에서 실행**합니다. 설치 담당자의 PC는 명령 실행과 브라우저 접속에 사용합니다. 별도 NetworkDoctor SaaS에 고객 Prometheus를 연결하는 구조가 아닙니다.

```text
노드의 에이전트 → 고객 Prometheus → Alertmanager → NetworkDoctor 백엔드
  ├─ Slack: 인시던트마다 원글 1개 + Grafana 링크
  └─ Holmes 분석 → 같은 원글의 댓글 + Incident UI 링크
                                      └─ 상세 보고서 → PDF 다운로드(사용자 PC)
```

UI 파일과 API는 같은 백엔드에서 제공하므로 UI 전용 서버를 따로 설치하지 않습니다. Grafana는 별도 프로그램이며 기존 Grafana 계정으로 로그인합니다. PDF 파일은 서버에 쌓지 않고, 인시던트 이력·진단 결과·Slack 발송 상태만 PVC에 보관합니다.

## 1. 먼저 설치 방식을 고르기

| 현재 환경 | 진행 방법 |
| --- | --- |
| 처음 설치하거나 Helm으로 직접 관리 | 이 문서의 준비 → Secret → values → Helm 설치 순서 |
| Argo CD Application이 이미 관리 | 준비·Secret·values 내용은 참고하되, **Git에 설정 반영 → 해당 Application을 수동 Sync**. 직접 Helm 설치·업그레이드를 겹쳐 실행하지 않음 |
| 설치는 끝났고 팀원이 접속만 함 | 설치 담당자가 전달한 UI·Grafana 주소 사용. 로컬 Helm·Docker 설치 불필요 |

명령은 저장소 루트에서 실행합니다. `YOUR_…` 값은 실제 환경 값으로 바꿉니다. 토큰 입력 블록은 **bash**용이며, macOS 기본 zsh라면 먼저 `bash`를 실행합니다.

```bash
git clone https://github.com/weeeeestern/NetworkDoctor.git
cd NetworkDoctor
kubectl config current-context
kubectl get nodes -o wide
helm list -A
kubectl get crd applications.argoproj.io
# 위 CRD가 있을 때만:
kubectl -n argocd get applications
```

Argo CD는 Helm을 렌더링 도구로 사용하므로, 관리 중이어도 `helm list`에는 나오지 않을 수 있습니다. 랩의 현재 설정은 자동 Sync가 아닙니다. main 병합·이미지 업로드만으로 배포되지는 않습니다. [Argo CD 절차](../deploy/argocd/README.md), [앱 목록과 소스 리비전](../deploy/bootstrap/onprem-lab.yaml)을 먼저 확인합니다.

## 2. 설치 전 필요한 구성

| 준비물 | 확인할 내용 |
| --- | --- |
| 명령 실행 환경 | 대상 클러스터에 접근하는 `kubectl`, Helm 3, Git. 직접 빌드할 경우 Docker/buildx, 아래 이미지 Secret 예제는 Python 3 필요 |
| 에이전트 노드 | Linux 커널 5.8+·BTF, 실제 NIC 이름, privileged·hostNetwork 허용. 현재 빌드 예제는 `linux/amd64` 대상 |
| Prometheus·Alertmanager | 수집·룰·웹훅 수신이 동작하는 기존 스택. 아래 values 예제는 Prometheus Operator CRD 사용 |
| Grafana | Prometheus 데이터 소스와 대시보드 sidecar가 동작하는 Grafana |
| 스토리지 | 사용 가능한 StorageClass 또는 기존 PVC. NetworkDoctor 차트는 스토리지 공급자를 설치하지 않음 |
| Holmes·LLM | 별도 Holmes 차트와 접근 가능한 LLM 게이트웨이·가상키. 분석 댓글에 필요 |
| Slack | 워크스페이스·채널·봇, 백엔드에서 `slack.com`으로 나가는 HTTPS 통신 |

```bash
kubectl get storageclass
kubectl get crd servicemonitors.monitoring.coreos.com \
  prometheusrules.monitoring.coreos.com alertmanagerconfigs.monitoring.coreos.com
kubectl get pods,svc -n monitoring
kubectl get prometheus,alertmanager -n monitoring -o yaml
```

**NetworkDoctor 차트는 Prometheus·Grafana·Holmes를 함께 설치하지 않습니다.** 모니터링이 없다면 [kube-prometheus-stack 설치 안내](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)를 따라 먼저 준비합니다. 기존 스택을 두 번째로 설치하지 않습니다. Prometheus의 monitor/rule 선택 라벨·네임스페이스, Alertmanager의 AlertmanagerConfig 선택 조건이 NetworkDoctor 리소스를 포함해야 합니다.

NIC·Cilium 지표 등 노드별 사전 확인은 [온프레미스 상세 점검](deploy-onprem.md#0-discovery-run-before-choosing-values)을 따릅니다. `values-onprem-lab.yaml`에는 팀 랩의 `eno1`과 내부 주소가 들어 있으므로, 다른 클러스터에 그대로 적용하지 않습니다.

이 문서의 대시보드 설치 경로는 **현재 차트의 ConfigMap + Grafana sidecar**입니다. sidecar가 대시보드 ConfigMap의 네임스페이스와 라벨(`grafana_dashboard: "1"`)을 읽는지 확인합니다. Grafana Operator만 설치된 환경에서 ConfigMap을 자동으로 가져온다고 가정하면 안 됩니다. Operator 설치·CR 적용은 별도 배포 경로를 확인해야 합니다.

### Holmes가 아직 없다면

LLM 게이트웨이는 먼저 별도로 준비합니다. [게이트웨이 설치 문서](../deploy/llm-gateway/README.md)를 참고하고, 실제 모델 키는 게이트웨이에, 가상키만 Kubernetes에 둡니다. 기존 Holmes가 있다면 새로 설치하지 말고 해당 주소·모델 이름을 사용합니다.

```bash
kubectl create namespace holmesgpt --dry-run=client -o yaml | kubectl apply -f -
read -r -s -p 'LLM gateway virtual key: ' nd_gateway_key
printf '\n'
printf '%s' "$nd_gateway_key" |
  kubectl -n holmesgpt create secret generic holmes-llm \
    --from-file=LLM_GATEWAY_KEY=/dev/stdin
unset nd_gateway_key
```

로컬 `holmes-values.yaml` 예시입니다. URL과 모델 별칭은 자신의 게이트웨이에 맞춥니다. 아래 `gpt-luna` 등은 이 프로젝트 게이트웨이의 별칭이며 임의의 공급자 모델 이름을 자동 변환하지 않습니다.

```yaml
holmes:
  modelList:
    gateway-luna:
      api_base: http://YOUR_LLM_GATEWAY:4000/v1
    gateway-sonnet:
      api_base: http://YOUR_LLM_GATEWAY:4000/v1
    gateway-haiku:
      api_base: http://YOUR_LLM_GATEWAY:4000/v1
  toolsets:
    prometheus/metrics:
      config:
        prometheus_url: http://prometheus-stack-kube-prom-prometheus.monitoring.svc:9090
```

```bash
helm lint deploy/helm/holmesgpt -f holmes-values.yaml
helm upgrade --install holmesgpt deploy/helm/holmesgpt \
  -n holmesgpt -f holmes-values.yaml --wait --timeout 10m
kubectl -n holmesgpt get deploy,svc,pods
```

이 차트의 의존 차트는 `charts/`에 포함되어 있습니다. 기본 `.invalid` URL을 그대로 사용하지 않습니다. 스킬은 파드 시작 시 Git에서 가져오므로 저장소 접근도 필요합니다. [Holmes 설정 상세](../deploy/helm/holmesgpt/README.md)

## 3. 이미지 준비와 Private 저장소 권한

Slack·UI 기능은 [PR #27](https://github.com/weeeeestern/NetworkDoctor/pull/27)로 main에 병합되어 있습니다. 배포할 이미지는 그 변경을 포함해야 합니다. 현재 랩의 고정 이미지 태그는 [랩 values](../deploy/helm/networkdoctor/values-onprem-lab.yaml)에서 확인할 수 있습니다. 이미 준비된 적절한 이미지를 쓴다면 직접 빌드는 생략합니다.

직접 빌드한다면 Docker Hub에 저장소를 **Private으로 먼저 생성**하고 로그인합니다. 아래는 백엔드 예제입니다. 에이전트까지 직접 빌드하려면 같은 방식으로 `build/agent/Dockerfile`을 사용합니다.

```bash
docker login
ND_IMAGE_REPO="docker.io/YOUR_DOCKERHUB_ID/networkdoctor-backend"
ND_IMAGE_TAG="manual-$(git rev-parse --short HEAD)"
docker buildx build --platform linux/amd64 \
  -f build/backend/Dockerfile \
  -t "$ND_IMAGE_REPO:$ND_IMAGE_TAG" --push .
```

**PC의 `docker login`은 Kubernetes 노드의 다운로드 권한이 아닙니다.** Private 이미지를 받으려면 아래 Secret도 필요합니다. Docker Hub에서 읽기 권한만 있는 별도 PAT를 발급합니다. 이 값은 Slack 봇 토큰과 다릅니다. 기존 Harbor가 있다면 그 저장소와 자격 증명을 사용할 수 있으며, Docker Hub Private 사용을 위해 Harbor를 추가 설치할 필요는 없습니다.

아래 예제는 새 Secret을 만듭니다. 이미 있으면 `AlreadyExists`를 확인하고 기존 것을 사용하거나 별도 교체 절차를 따릅니다. 토큰을 Git·values·채팅·명령 인자에 넣지 않습니다.

```bash
kubectl create namespace networkdoctor --dry-run=client -o yaml | kubectl apply -f -
export ND_REGISTRY_USER="YOUR_DOCKERHUB_ID"
read -r -s -p 'Docker Hub read-only PAT: ' nd_registry_token
printf '\n'
set -o pipefail
printf '%s' "$nd_registry_token" |
  python3 -c 'import os,sys,json,base64; a=base64.b64encode((os.environ["ND_REGISTRY_USER"]+":"+sys.stdin.read()).encode()).decode(); print(json.dumps({"auths":{"https://index.docker.io/v1/":{"auth":a}}}))' |
  kubectl -n networkdoctor create secret generic networkdoctor-registry \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson=/dev/stdin
unset nd_registry_token ND_REGISTRY_USER
```

Secret은 이미지를 사용하는 파드와 **같은 네임스페이스**에 있어야 합니다. 백엔드는 `backend.imagePullSecrets`, 에이전트가 Private이면 `agent.imagePullSecrets`에도 지정합니다. PAT 만료 후 새 노드나 재생성된 파드에서 다운로드가 실패할 수 있으므로 만료일과 교체 담당자를 관리합니다. [Kubernetes Private 이미지 안내](https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/)

## 4. Slack 봇 연결과 토큰 Secret

**봇 설치는 처음 한 번**, 장애 알림과 댓글은 이후 자동입니다. Slack 앱은 `chat:write` 권한으로 워크스페이스에 설치하고, 알림 채널에 봇을 초대합니다. 앱의 Bot User OAuth Token(`xoxb-…`)과 채널 ID를 확인합니다. 팀 채널은 `network doctor` 워크스페이스의 `#incident`이며, 다른 환경에서는 자신의 채널 ID를 사용합니다.

화면별 절차는 [Slack 연동 가이드 2~4절](slack-integration.ko.md#2-slack-앱-생성과-설치--최초-1회)에 있습니다. Incoming Webhook·이벤트 수신 서버·별도 Slack 봇 프로그램은 필요하지 않습니다. NetworkDoctor 백엔드가 직접 원글과 스레드 댓글을 보냅니다.

```bash
kubectl create namespace networkdoctor --dry-run=client -o yaml | kubectl apply -f -
read -r -s -p 'Slack Bot User OAuth Token: ' nd_slack_token
printf '\n'
printf '%s' "$nd_slack_token" |
  kubectl -n networkdoctor create secret generic networkdoctor-slack \
    --from-file=bot-token=/dev/stdin
unset nd_slack_token
```

여기서 **토큰은 봇이 Slack에 접근하는 열쇠**, **Secret은 그 열쇠를 Kubernetes에 보관하는 객체**입니다. Helm에는 토큰 대신 Secret 이름만 넣습니다. 기존 Secret의 토큰을 바꾼 경우 백엔드를 재시작해야 환경 변수에 반영됩니다.

## 5. values 작성 후 Helm 설치

아래는 **기존 kube-prometheus-stack과 Holmes를 사용하는 첫 설치** 예제입니다. 로컬 `customer-values.yaml`로 저장하고 NIC·라벨·서비스 주소·StorageClass·이미지·채널 ID를 바꿉니다. 기존 설치를 업그레이드한다면 기존 values와 PVC 설정을 보존하고 필요한 값만 덮어씁니다.

```yaml
global:
  clusterName: YOUR_CLUSTER_NAME
agent:
  iface: YOUR_WORKER_NIC
  # 일반 워커만 대상으로 시작. 노드별 NIC와 taint가 다르면 별도 설계 필요.
  tolerations: []
serviceMonitor:
  enabled: true
  labels:
    release: prometheus-stack
prometheusRule:
  enabled: true
  labels:
    release: prometheus-stack
alertmanagerConfig:
  enabled: true
grafanaDashboards:
  enabled: true
  namespace: monitoring
backend:
  replicaCount: 1
  image:
    repository: docker.io/YOUR_DOCKERHUB_ID/networkdoctor-backend
    tag: YOUR_IMAGE_TAG
  imagePullSecrets:
    - name: networkdoctor-registry
  config:
    prometheusURL: http://prometheus-stack-kube-prom-prometheus.monitoring.svc:9090
    holmesURL: http://holmesgpt-holmes.holmesgpt.svc:80
    holmesModel: gateway-luna
    holmesAuto: "true"
    arch: derived
  persistence:
    enabled: true
    storageClass: YOUR_STORAGE_CLASS
    size: 1Gi
slack:
  enabled: true
  channelID: YOUR_CHANNEL_ID
  existingSecret: networkdoctor-slack
  botTokenKey: bot-token
# 같은 PC에서 포트 연결로 시험할 때의 예시입니다.
ui:
  url: http://127.0.0.1:18081
grafana:
  url: http://127.0.0.1:13000
```

공개 이미지를 쓴다면 불필요한 `imagePullSecrets`를 제거합니다. 기존 PVC는 `backend.persistence.existingClaim`으로 지정할 수 있습니다. 백엔드는 파일 저장 방식이므로 **1레플리카 + PVC + Recreate**를 유지합니다. Slack 활성화 시 차트도 이 조건을 검사합니다.

```bash
helm lint deploy/helm/networkdoctor -f customer-values.yaml
helm template networkdoctor deploy/helm/networkdoctor \
  -n networkdoctor -f customer-values.yaml > /tmp/networkdoctor-rendered.yaml
# 렌더링 결과를 확인한 뒤 설치:
helm upgrade --install networkdoctor deploy/helm/networkdoctor \
  -n networkdoctor --create-namespace -f customer-values.yaml \
  --wait --timeout 10m
kubectl -n networkdoctor get ds,deploy,pods,svc,pvc
kubectl -n networkdoctor rollout status deployment/networkdoctor-backend
kubectl -n networkdoctor rollout status daemonset/networkdoctor-agent
```

위 차트 주소는 Git 안의 로컬 경로입니다. 공개 Helm 저장소에 대한 `helm repo add networkdoctor …`는 현재 배포 절차에 없습니다.

차트에서 `AlertmanagerConfig`를 만들면 같은 알림 경로를 수동으로 중복 추가하지 않습니다. Alertmanager에서도 직접 Slack으로 보내고 있다면 이중 알림이 생기지 않도록 대상을 조정합니다. CRD가 없는 환경에서는 [대체 수집·룰 설정](deploy-onprem.md#2-install)과 [Alertmanager 설정](../deploy/alertmanager/README.md)이 별도로 필요합니다.

**Argo CD인 경우:** 위 `helm upgrade`는 실행하지 않습니다. 기존 Application이 읽는 values에 같은 설정을 커밋하고, 소스 리비전을 확인한 뒤 대상 자식 Application을 수동 Sync합니다. 로컬에 만든 `customer-values.yaml`은 Argo CD에 자동 반영되지 않습니다. 자식 앱 추가나 소스 리비전·valueFiles·대상 등 Application 정의를 바꿨다면 먼저 root를 Sync합니다. 일반 values 내용만 바꿨다면 자식 Sync로 충분합니다. Secret은 Git 밖에서 미리 만듭니다.

## 6. UI·Grafana 접속을 열고 URL 확인하기

**`ui.url`·`grafana.url`은 메시지에 넣을 링크를 정하는 설정입니다. 서비스 공개·DNS·HTTPS를 설정하지는 않습니다.** 현재 Helm NOTES에는 UI·Grafana URL이 출력되지 않습니다. 설치 담당자가 아래 접속 경로를 마련하고 브라우저에서 열리는지 확인한 뒤 팀에 URL을 전달합니다.

### 같은 PC에서 시험 — PC에서 kubectl을 사용할 수 있는 경우

별도 터미널 두 개에서 다음 명령을 계속 실행해 둡니다. Grafana 서비스 이름·포트는 `kubectl -n monitoring get svc`로 실제 값을 확인합니다.

```bash
# 터미널 1:
kubectl -n networkdoctor port-forward svc/networkdoctor-backend 18081:8080
```

```bash
# 터미널 2(Grafana Service가 80번인 경우):
kubectl -n monitoring port-forward svc/prometheus-stack-grafana 13000:80
```

이 PC의 브라우저에서 UI는 `http://127.0.0.1:18081/#/`, Grafana는 `http://127.0.0.1:13000/`를 엽니다. 위 values는 이 조합에 맞춘 것입니다. 터미널이나 VPN을 닫으면 접속이 끊깁니다. 파드를 다시 만들었을 때는 포트 연결을 재실행해야 할 수 있습니다.

### 온프레미스 접속 서버에서만 kubectl을 사용할 수 있는 경우

Mac의 별도 터미널 두 개에서 실행합니다. `YOUR_CP_ADDRESS`는 SSH로 접속할 수 있는 주소로 바꿉니다. Mac의 로컬 포트와 접속 서버의 포트를 다르게 지정했습니다.

```bash
# 터미널 1: Mac 18081 → 접속 서버 18181 → UI
ssh -o ExitOnForwardFailure=yes \
  -L 127.0.0.1:18081:127.0.0.1:18181 cpadmin@YOUR_CP_ADDRESS \
  'kubectl -n networkdoctor port-forward svc/networkdoctor-backend 18181:8080'
```

```bash
# 터미널 2: Mac 13000 → 접속 서버 13100 → Grafana
ssh -o ExitOnForwardFailure=yes \
  -L 127.0.0.1:13000:127.0.0.1:13100 cpadmin@YOUR_CP_ADDRESS \
  'kubectl -n monitoring port-forward svc/prometheus-stack-grafana 13100:80'
```

브라우저에서 여는 URL은 앞 항목과 같습니다. Slack에 localhost 링크를 보내도 **다른 사람 PC로 연결되는 것은 아닙니다.** 각자 같은 포트 연결을 만들거나, 아래 팀 공용 URL을 사용합니다.

### 팀에서 사용 — 실제로 접속 가능한 공용 URL

관리자가 기존 Ingress·로드밸런서 또는 LAN/VPN 안의 NodePort를 준비합니다. 현재 차트에는 Ingress 템플릿이나 로그인 기능이 없으므로, UI를 인터넷에 공개하기 전에 인증과 HTTPS를 별도로 준비합니다.

NodePort를 쓰려면 values에 `backend.service.type: NodePort`를 추가하고 선택한 관리 방식으로 다시 반영합니다. 현재 템플릿은 고정 nodePort 값을 받지 않으므로 할당된 번호를 확인합니다.

```bash
kubectl -n networkdoctor get svc networkdoctor-backend \
  -o jsonpath='{.spec.ports[0].nodePort}{"\n"}'
kubectl get nodes -o wide
```

접속 가능한 노드 IP와 해당 포트를 합친 `http://NODE_IP:NODE_PORT`가 UI의 기본 URL입니다. Grafana는 Grafana 쪽 차트·Service에서 공개하고 그 URL을 확인합니다. 클러스터 내부 `.svc` 주소를 브라우저용으로 쓰지 않습니다. 방화벽·VPN을 통한 접속 가능 여부도 확인합니다.

확인한 주소를 `ui.url`·`grafana.url`에 설정하고 다시 반영합니다. `ui.url`은 사이트 루트만, `grafana.url`은 실제 Grafana의 하위 경로도 지정할 수 있습니다. 둘 다 HTTP(S)를 사용하며 인증 정보·쿼리·`#`는 넣지 않습니다. 비어 있으면 해당 링크를 숨깁니다. `netdoc.cloud`도 DNS·공개 경로·TLS를 준비한 뒤 실제 URL을 설정해야 합니다.

설정된 URL 두 개만 확인하는 명령입니다. 토큰은 표시하지 않습니다.

```bash
kubectl -n networkdoctor get deployment networkdoctor-backend \
  -o jsonpath='{range .spec.template.spec.containers[?(@.name=="backend")].env[?(@.name=="ND_PUBLIC_URL")]}UI: {.value}{"\n"}{end}{range .spec.template.spec.containers[?(@.name=="backend")].env[?(@.name=="ND_GRAFANA_URL")]}Grafana: {.value}{"\n"}{end}'
```

Grafana는 기존 관리자가 마련한 계정으로 로그인합니다. NetworkDoctor UI를 열었다고 Grafana에도 로그인되는 것은 아닙니다. 설치 담당자는 팀에 **UI URL·Grafana URL·필요한 VPN·Grafana 계정 이용 방법**을 전달합니다. 새 장애의 Slack 알림에도 아래 개별 링크가 도착합니다.

## 7. Slack 원글 → 분석 댓글 → UI → PDF 확인

1. UI 목록이 열리고, Grafana에서 `nd-rule-1`~`nd-rule-8`이 표시되며 데이터 소스로부터 그래프를 가져오는지 확인합니다.
2. 테스트 클러스터에서 새 Rule 7 장애를 발생시킵니다. Slack의 `Incident detected` 원글과 `Open Grafana` 링크를 확인합니다.
3. Holmes가 분석을 완료하면 **같은 원글의 스레드**에 `Analysis ready`와 `View latest report`가 도착하는지 확인합니다.
4. UI 상세에서 증상·추정 원인·근거·배제한 원인·조치를 확인하고 PDF 다운로드를 누릅니다. PC에 저장된 PDF를 열어 페이지를 넘기며 글이 누락되는지 확인합니다.

Grafana 링크는 `…/d/nd-rule-N?from=…&to=…&var-node=…`, 보고서 링크는 `…/#/incident/인시던트ID`입니다. 백엔드가 발생 시각·노드로 생성하므로 사용자가 매번 URL을 조립할 필요는 없습니다.

### Rule 7을 재현할 경우 — 랩 전용

Cat Shop이 아직 없다면 데모 차트를 추가합니다. Argo CD가 관리하는 기존 데모에 중복 설치하지 않습니다. 데모의 ServiceMonitor 라벨도 Prometheus의 선택 조건에 맞춥니다.

```bash
helm upgrade --install networkdoctor-demo deploy/helm/networkdoctor-demo \
  -n networkdoctor-demo --create-namespace \
  --set serviceMonitor.labels.release=prometheus-stack --wait --timeout 5m
# kubectl·python3·curl·timeout을 사용할 수 있는 Linux 접속 서버에서 실행:
bash scripts/repro/rule7-app-5xx.sh
```

PC에서 스크립트를 복사하는 방법·전제는 [재현 스크립트 설명](../scripts/repro/README.md)에 있습니다. Rule 7은 Cat Shop에 약 7분간 5xx 요청을 보내고 이후에도 분석을 기다립니다. 룰의 지속 시간 판정·Alertmanager 대기·분석 큐·LLM 처리가 있어 즉시 알림이 오지는 않습니다. 이전 장애가 해소된 뒤 기본 15분의 안정 기간을 확인합니다. `PASS`는 인시던트·분석 확인이며, Slack 도착·링크·PDF 화면은 별도로 확인해야 합니다.

중단하려면 스크립트를 **Ctrl+C**로 멈추고 정리가 끝나기를 기다립니다. Rule 7 재현 파드가 남았는지 확인하고, 남은 경우에만 대상을 명시하여 삭제합니다.

```bash
kubectl -n networkdoctor-demo get pods -l networkdoctor.io/repro=true
# 이번 Rule 7 테스트 파드가 남아 있을 때만:
kubectl -n networkdoctor-demo delete pod nd-r7-client --ignore-not-found
```

재현 파드가 없어지고 Grafana에서 새로운 5xx가 늘지 않는지 확인한 뒤 VPN을 끕니다. **VPN 종료는 장애 주입을 멈추는 작업이 아닙니다.** 일반 모니터링·백엔드와 테스트의 장애 주입은 별개입니다. Cat Shop의 일반 loadgen은 정상 요청을 계속 보냅니다. Rule 7 스크립트는 종료 시 재현 파드를 삭제하며 NIC 설정을 바꾸지 않습니다. 다른 룰은 변경 내용·복구 절차가 다르므로 해당 설명을 먼저 읽습니다.

## 8. 문제가 생겼을 때

| 증상 | 먼저 확인할 곳 |
| --- | --- |
| `ImagePullBackOff` | 실제 이미지 이름·태그, Private 여부, 같은 namespace의 pull Secret, PAT 만료일. `kubectl describe pod`의 Events |
| PVC가 Pending | StorageClass·용량·기존 PVC 지정. 차트를 설치해도 스토리지 공급자가 생기지는 않음 |
| UI가 열리지 않음 | Service 공개 여부, 포트 연결 종료·포트 충돌, VPN, 실제 배포 이미지가 UI를 포함하는지 |
| Grafana가 404/No data | UID, sidecar의 대상 namespace·라벨, Prometheus 데이터 소스, 시각·노드 변수 |
| Slack에 아무것도 오지 않음 | 활성화 후 새로 생성된 대상 firing인지. `smoke-*`·과거 이력은 발송 대상 제외. 봇 초대·채널 ID·Secret·외부 HTTPS도 확인 |
| 원글만 오고 댓글이 없음 | Holmes 연결·키·모델, 최신 분석 상태. 재시작으로 중단된 분석은 UI에서 재분석 |
| Slack에 링크가 없음 | 빈 URL은 의도적으로 숨김. Grafana 링크는 rule-1~8 대상. URL 설정 후 재반영 |

자세한 재시도·불명확한 발송 상태 처리는 [Slack 연동 장애 대응](slack-integration.ko.md#9-실패재시작-시-처리)을 참고합니다. 수동으로 메시지를 반복 게시하거나 가동 중인 상태 파일을 편집하지 않습니다.

PDF의 근거 본문도 확인 대상에 포함합니다. 현재 긴 Source query URL의 표시가 페이지 끝에서 잘리는 알려진 제한이 있습니다(연결 대상의 전체 URL은 유지). PDF를 공유할 때 이 표시도 확인합니다. PDF 저장용 PVC·서버 PDF 저장 처리는 필요하지 않습니다.
