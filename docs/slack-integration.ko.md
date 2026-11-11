# NetworkDoctor Slack 연동 가이드

처음 설치한다면 [Helm 설치부터 접속·Slack·PDF까지](install-and-access.ko.md)를 먼저 읽습니다. Private 이미지 다운로드 권한, Holmes 준비, Helm/Argo CD 구분, UI·Grafana URL 확인과 포트 연결을 포함합니다. 이 문서는 Slack 앱 설정과 알림 동작을 자세히 설명합니다.

## 설치와 알림 발송은 언제 실행되나요?

**봇 설치와 권한 승인은 처음 연결할 때 한 번 진행합니다.** 설치한 봇의 토큰과 채널 ID를 백엔드에 설정하면, 이후 장애 알림과 분석 완료 댓글은 백엔드가 자동으로 전송합니다. 알림을 보낼 때마다 사람이 승인하는 구조가 아닙니다.

| 시점 | 필요한 작업 |
| --- | --- |
| 처음 연결 | Slack 앱 생성, 발송 권한 승인, 워크스페이스 설치, 채널에 봇 추가, 토큰 Secret 등록 |
| 같은 설정으로 백엔드 재배포 | 기존 앱과 Secret 재사용 |
| 새 장애 발생 | 백엔드가 해당 인시던트의 원글 전송 |
| Holmes 분석 완료 | 백엔드가 같은 원글의 스레드에 결과와 UI 링크 전송 |
| 앱 권한 변경 | 변경한 권한에 대한 Slack 재설치·재승인이 필요할 수 있음 |
| 토큰 교체 | Kubernetes Secret 갱신 후 백엔드 재시작 |

다른 고객의 워크스페이스에 설치할 때는 그 워크스페이스에서도 설치 승인을 받아야 합니다. Slack 앱 설정의 **OAuth & Permissions**는 봇의 Slack 접근 권한을 설정하는 메뉴입니다.

이 문서는 연동 절차를 설명합니다. 문서 추가나 Git 푸시 자체가 Slack 앱 설치, Kubernetes 배포 또는 실제 알림 발송을 실행하지는 않습니다.

## 1. 준비할 것

- Slack 워크스페이스와 알림 채널, 해당 워크스페이스에 앱을 설치할 수 있는 권한
- 이 기능이 포함된 NetworkDoctor 백엔드 이미지
- 실행 중인 백엔드 **1개**와 인시던트 저장용 **영속 볼륨(PVC)**
- Prometheus → Alertmanager → NetworkDoctor 백엔드 웹훅 연결
- 분석 완료 댓글을 받으려면 동작하는 Holmes 연동
- 백엔드에서 `https://slack.com/api/chat.postMessage`로 나가는 HTTPS 통신
- 링크를 표시하려면 사용자의 브라우저에서 열리는 Grafana·Incident UI 주소

이번 팀 시연에서 선택한 대상은 **`network doctor` 워크스페이스의 `#incident` 채널**입니다. 다른 환경에서는 아래 예시의 채널 ID와 주소를 해당 환경에 맞게 바꿉니다.

Slack 봇 준비는 EKS 생성 전에도 할 수 있습니다. 백엔드 연결과 실제 알림 확인은 Kubernetes 배포 후 진행합니다.

## 2. Slack 앱 생성과 설치 — 최초 1회

1. [Slack 앱 관리 화면](https://api.slack.com/apps)에 로그인합니다.
2. **Create New App**을 누릅니다. 기능을 직접 고르는 **From scratch / Pick features** 경로를 선택합니다.
3. 앱 이름을 `NetworkDoctor`, 워크스페이스를 실제 알림을 받을 워크스페이스로 지정합니다.
4. **Bot scopes**에 **`chat:write`** 하나를 추가합니다. User scopes와 이벤트 구독은 추가하지 않습니다.
5. 설정을 검토하고 **Create and Install**을 진행합니다. Slack의 권한 승인 화면이 나오면 대상 워크스페이스와 발송 권한을 확인하고 승인합니다.
6. 앱 설정의 **OAuth & Permissions**에서 **Bot User OAuth Token**을 확인합니다. 일반적인 봇 토큰은 `xoxb-`로 시작합니다.

이미 만든 앱을 사용하는 경우에는 **OAuth & Permissions → Bot Token Scopes**에서 `chat:write`를 설정하고, **Install to Workspace / Reinstall**로 설치·승인합니다. 화면 버전에 따라 버튼 이름은 다를 수 있습니다. 앱 설치 정책에 따라 워크스페이스 관리자의 승인이 필요할 수도 있습니다.

현재 백엔드는 이 토큰으로 Slack의 `chat.postMessage` API를 호출합니다. Slack의 이벤트 수신 서버, Request URL, Socket Mode 또는 별도 Bolt 프로그램을 띄우는 작업은 필요하지 않습니다. [Slack 발송 API 문서](https://docs.slack.dev/reference/methods/chat.postMessage/)

## 3. 알림 채널에 봇 추가

1. Slack에서 알림 채널을 엽니다. 이번 팀 시연에서는 `#incident`입니다.
2. 채널의 앱 추가 기능으로 `NetworkDoctor`를 추가하거나, 채널에서 `/invite @NetworkDoctor`를 실행합니다.
3. 채널 제목을 눌러 세부정보에서 **채널 ID**를 복사합니다. Helm에는 `#incident`라는 이름 대신 채널 ID를 넣습니다.

현재 팀 시연에서 확인한 `#incident`의 채널 ID는 `C0CG7301YEQ`입니다. 다른 워크스페이스나 채널에서는 반드시 자신의 채널 ID를 사용합니다. 채널에 봇을 추가하는 작업은 최초 연결 시 진행하며, 매번 알림을 받을 때 반복하지 않습니다.

## 4. 봇 토큰을 Kubernetes Secret으로 등록

프로그램을 설치할 클러스터를 선택한 터미널에서 실행합니다. 다음 명령은 백엔드 네임스페이스가 `networkdoctor`이고 이미 존재한다고 가정합니다.

먼저 현재 대상을 확인합니다.

```bash
kubectl config current-context
kubectl get namespace networkdoctor
```

아래 토큰 입력 명령은 **bash 기준**입니다. macOS의 기본 zsh를 사용 중이라면 먼저 `bash`를 실행한 뒤, 새 프롬프트에서 다음 블록을 실행합니다. 입력한 토큰은 화면에 표시되지 않습니다.

```bash
read -r -s -p 'Slack Bot User OAuth Token: ' nd_demo_bot_token
printf '\n'
printf '%s' "$nd_demo_bot_token" |
  kubectl -n networkdoctor create secret generic networkdoctor-slack \
    --from-file=bot-token=/dev/stdin
unset nd_demo_bot_token
```

생성되는 Secret 이름은 `networkdoctor-slack`, 토큰을 담는 키는 `bot-token`입니다. 백엔드와 같은 네임스페이스에 있어야 합니다. 차트는 이 Secret을 참조하며 토큰을 자동으로 발급하거나 Secret을 생성하지 않습니다.

토큰은 채팅, 문서, Git 저장소, Helm values 또는 명령의 인자에 적지 않습니다. `AlreadyExists`가 나오면 기존 Secret이 있는 상태이므로 새로 만드는 명령을 반복하지 말고 사용할 Secret을 확인합니다. 토큰을 교체했다면 Secret을 갱신한 후 다음과 같이 백엔드를 재시작합니다. 릴리스 이름을 바꿨다면 Deployment 이름도 실제 이름으로 바꿉니다.

```bash
kubectl -n networkdoctor rollout restart deployment/networkdoctor-backend
kubectl -n networkdoctor rollout status deployment/networkdoctor-backend
```

## 5. 기능이 포함된 백엔드 이미지 준비

이 기능은 [PR #27](https://github.com/weeeeestern/NetworkDoctor/pull/27)로 main에 병합되었습니다. Git 병합이나 브랜치 푸시만으로 기존 클러스터의 백엔드 이미지가 교체되지는 않습니다. 기능을 포함하는 배포 이미지를 쓰거나, 해당 리비전에서 고유한 태그로 이미지를 빌드·업로드한 뒤 Helm에 지정합니다.

아래 `YOUR_REGISTRY`를 자신이 푸시할 수 있는 저장소로 바꿉니다. Docker와 해당 저장소 로그인이 준비되어 있어야 합니다. 현재 백엔드 배포 이미지의 대상 아키텍처는 `linux/amd64`입니다.

```bash
ND_SLACK_DEMO_REPO="YOUR_REGISTRY/networkdoctor-backend"
ND_SLACK_DEMO_TAG="ux-$(git rev-parse --short HEAD)"
docker buildx build --platform linux/amd64 \
  -f build/backend/Dockerfile \
  -t "$ND_SLACK_DEMO_REPO:$ND_SLACK_DEMO_TAG" --push .
```

현재 랩의 고정 이미지 태그는 `deploy/helm/networkdoctor/values-onprem-lab.yaml`에서 확인합니다. 이전 설치가 오래된 이미지를 사용한다면 업그레이드가 필요합니다. Private 저장소를 쓰면 PC 로그인과 별도로 Kubernetes의 `imagePullSecrets`도 설정합니다. [이미지·다운로드 권한 준비](install-and-access.ko.md#3-이미지-준비와-private-저장소-권한)

## 6. Helm 설정과 배포

로컬에 `slack-demo-values.yaml`을 만듭니다. 아래 `YOUR_CHANNEL_ID`, 이미지 저장소와 태그를 실제 값으로 바꿉니다. 팀 시연 채널은 위에서 확인한 `C0CG7301YEQ`를 사용합니다.

```yaml
slack:
  enabled: true
  channelID: "YOUR_CHANNEL_ID"
  existingSecret: networkdoctor-slack
  botTokenKey: bot-token
grafana:
  url: ""
ui:
  url: ""
backend:
  replicaCount: 1
  image:
    repository: YOUR_REGISTRY/networkdoctor-backend
    tag: YOUR_UNIQUE_TAG
  persistence:
    enabled: true
```

`grafana.url`과 `ui.url`에는 브라우저에서 실제로 열리는 주소를 넣습니다. 예를 들어 NodePort를 사용한다면 접근 가능한 노드 IP와 실제 할당된 포트가 필요합니다.

- 빈 주소는 Slack과 UI에서 해당 링크를 숨깁니다.
- `ui.url`은 `https://호스트명`처럼 사이트 루트 주소여야 합니다. 하위 경로는 지원하지 않습니다.
- `grafana.url`은 Grafana가 실제 제공되는 하위 경로를 포함할 수 있습니다.
- 두 주소 모두 HTTP(S)를 사용하며, 계정 정보·쿼리 문자열·`#` 조각은 넣지 않습니다.
- `.svc` 내부 주소는 사용자용 링크로 쓰지 않습니다. `localhost` 주소는 그 PC에서만 열립니다.
- 이 값은 링크 생성용입니다. Service 노출, Ingress, DNS, HTTPS 인증서 설정은 실제 배포 환경에서 준비해야 합니다. 도메인 구매만으로 접속 주소가 완성되지는 않습니다.

다음은 **Helm으로 직접 관리하는 기존 온프레미스 랩**의 예시입니다. EKS에서는 해당 클러스터에 맞는 values 파일을 사용해야 합니다. 온프레미스 랩의 NIC·내부 서비스 주소·스토리지 설정을 그대로 적용하지 않습니다.

```bash
helm lint deploy/helm/networkdoctor \
  -f deploy/helm/networkdoctor/values-onprem-lab.yaml \
  -f slack-demo-values.yaml
helm upgrade --install networkdoctor deploy/helm/networkdoctor \
  -n networkdoctor \
  -f deploy/helm/networkdoctor/values-onprem-lab.yaml \
  -f slack-demo-values.yaml
kubectl -n networkdoctor rollout status deployment/networkdoctor-backend
```

Argo CD로 관리하는 환경에서는 위 Helm 명령을 겹쳐 실행하지 않습니다. 기존 Application의 소스 리비전과 Git의 values에 기능 및 설정을 반영하고 수동 Sync합니다. 로컬 `slack-demo-values.yaml`은 Argo CD가 자동으로 읽지 않습니다. 배포 방식은 [기존 랩 설치 문서](deploy-onprem.md)를 참고합니다.

접속을 여는 명령과 설치 후 URL 확인 방법은 [UI·Grafana 접속 안내](install-and-access.ko.md#6-uigrafana-접속을-열고-url-확인하기)를 따릅니다. 현재 Helm NOTES는 UI·Grafana URL을 출력하지 않습니다. 설치 담당자가 실제 접속 경로를 마련한 뒤 팀에 주소를 전달합니다.

Slack의 기본값은 비활성화입니다. 활성화 시 차트는 백엔드가 1개이고 영속 볼륨을 사용하도록 검사합니다. 배포 전략은 기존 `Recreate`를 유지합니다. 인시던트 이력·분석 결과·Slack 발송 상태가 볼륨에 저장되며, PDF는 요청 시 브라우저에서 생성해 사용자 PC로 다운로드합니다.

## 7. 연결 후 실제 사용 흐름

```text
Prometheus 장애 감지 → Alertmanager → NetworkDoctor 인시던트 생성
  → #incident 원글: 증상 + Grafana 링크
  → Holmes 분석 완료
  → 같은 원글의 스레드: 분석 결과 + Incident UI 링크
  → 사용자가 UI에서 상세 확인·PDF 다운로드
```

1. 채널의 앱 목록에 봇이 있고, Grafana·UI 주소가 시연할 PC에서 열리는지 확인합니다. Grafana에는 해당 `nd-rule-1`~`nd-rule-8` 대시보드와 데이터가 있어야 합니다.
2. 준비된 테스트 클러스터에서 승인된 시나리오로 **새 장애**를 발생시킵니다. 실제 장애 주입은 대상과 복구 방법을 확인한 후 진행합니다.
3. `Incident detected` 원글과 설정한 Grafana 링크를 확인합니다.
4. Holmes가 사용 가능한 원인 분석을 완료하면 같은 스레드의 `Analysis ready` 댓글과 `View latest report` 링크를 확인합니다.
5. 링크로 UI를 열어 증상·원인·근거를 보고 PDF를 다운로드합니다.

이 과정에서 사람의 발송 승인은 필요하지 않습니다. 설치 후에는 백엔드가 저장한 봇 토큰을 사용합니다. Slack 앱을 PC에 계속 열어 둘 필요도 없습니다.

## 8. 알림이 예상과 다를 때

| 현상 | 확인할 내용 |
| --- | --- |
| 기존 장애에는 알림이 오지 않음 | 활성화 이후 처음 생성된, 알림 대상에 해당하는 firing 인시던트만 등록합니다. 과거 이력이나 처음부터 resolved로 받은 기록은 소급 발송하지 않습니다. |
| 테스트 알림이 오지 않음 | `smoke`로 시작하는 규칙과 설정된 조사 제외 접두사의 규칙은 알림 대상에서 제외됩니다. 실제 대상 규칙의 새 인시던트로 확인합니다. |
| 같은 장애에 원글이 두 번 옴 | Alertmanager에서도 같은 장애를 Slack으로 직접 보내는지 확인합니다. 해당 알림의 발송 경로를 NetworkDoctor로 정리합니다. |
| 원글은 오지만 분석 댓글이 없음 | Holmes 연결·조사 상태와 현재 분석의 `root_cause` 결과를 확인합니다. 분석 실패나 이전 결과만 남아 있는 상태에는 완료 댓글을 새로 보내지 않습니다. |
| 링크가 없음 | `grafana.url`·`ui.url`이 비어 있는지 확인합니다. Grafana 링크는 현재 rule-1~rule-8에 생성됩니다. |
| `not_in_channel` / `channel_not_found` | 봇이 대상 채널에 추가됐는지, 토큰과 채널이 같은 워크스페이스인지, 채널 ID가 맞는지 확인합니다. |
| `invalid_auth` / `token_revoked` / `missing_scope` | 봇 토큰·권한·Secret 이름과 키를 확인합니다. 수정한 Secret을 읽도록 백엔드를 재시작합니다. |

인시던트마다 원글 하나를 유지합니다. 같은 웹훅을 반복 수신해도 같은 인시던트의 발송 상태를 사용하며, 시작 시각(`startsAt`)이 다른 새 장애는 새 인시던트와 원글이 됩니다. 채널 설정을 바꾸면 이후 새로 등록되는 인시던트에 적용되고, 기존 인시던트는 처음 저장한 채널을 유지합니다.

여러 인시던트를 묶어 분석해도 원글과 댓글은 각 인시던트의 스레드에 붙습니다. 공유 분석은 관련 그룹 결과로 표시하며, 각 구성원의 개별 원인이 모두 검증됐다는 뜻은 아닙니다. 복구 알림을 별도로 전송하는 기능은 현재 없으며, 분석 댓글에는 메시지를 처음 준비한 시점의 해당 인시던트 복구 상태가 포함됩니다. 재시도 시에는 저장된 메시지 내용을 유지합니다. UI·보고서 링크는 최신 결과를 보여주므로 재분석 후 내용이 달라질 수 있습니다.

## 9. 실패·재시작 시 처리

보호된 데이터 볼륨의 인시던트 JSON에 `slack_notification` 발송 상태가 저장됩니다. 이 정보는 공개 상세 API 응답에서 제외됩니다.

- `pending` / `retry`: 백엔드가 자동으로 이어서 시도합니다. 채널당 호출 간격과 Slack의 429 `Retry-After`를 지킵니다.
- `failed`: 권한·채널·토큰 등의 명확한 거절입니다. 원인을 고친 후 발송 상태를 별도로 확인해야 합니다.
- `unknown` 또는 중단된 `sending`: Slack에 이미 게시됐을 수 있어 자동 재게시하지 않습니다. 재시작 시 남은 `sending`은 `unknown`으로 보류합니다.

`failed`나 `unknown`을 수동 복구할 때는 백엔드를 중지하고 해당 JSON을 백업한 후 채널과 스레드의 실제 게시 여부를 대조합니다. 게시가 확인되면 해당 전송만 `sent`로 기록하고 정확한 메시지 `ts`를 문자열로 저장합니다. 분석 댓글은 해당 전송의 `source_incident_id`·`source_attempt`를 상위 receipt의 `last_source_incident_id`·`last_source_attempt`에도 반영합니다. 미게시가 확실하고 원인이 해결된 경우에만 해당 전송을 `pending`으로 되돌리고 `next_attempt_at`·`last_error_code`를 제거합니다. 기존 채널·payload·source·부모 receipt는 유지합니다. 판단이 어려우면 `unknown`을 유지합니다. 수정 후 백엔드 1개를 다시 시작합니다.

백엔드 재시작으로 중단된 Holmes 조사는 실패 상태로 표시되고 이전 결과는 보존됩니다. UI의 **Reanalyze**로 수동 재분석합니다. 조사 대기열의 자동 재실행은 현재 지원하지 않습니다.

## 참고

- [설치·접속 통합 가이드](install-and-access.ko.md)
- [영문 시연 가이드](demo-slack.md)
- [Slack 앱 생성 공식 안내](https://docs.slack.dev/tools/bolt-js/creating-an-app/): 앱·권한·토큰 설정 부분 참고
- [Slack 메시지 발송 API](https://docs.slack.dev/reference/methods/chat.postMessage/)
- [Slack API 호출 제한](https://docs.slack.dev/apis/web-api/rate-limits/)
