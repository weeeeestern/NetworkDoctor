# NetworkDoctor — AWS EKS 시연 환경

서울 리전에 5일짜리 팀 시연 환경을 만든다. Terraform 스택은 두 개다.

- `envs/dns` — Route53 공개 영역(`netdoc.cloud`)과 와일드카드 ACM 인증서.
  **한 번 만들고 유지한다.** 랩을 지워도 네임서버와 인증서가 바뀌지 않는다.
- `envs/eks-lab` — VPC, EKS, NAT Gateway, Cilium, AWS Load Balancer Controller,
  external-dns, Gateway, Bedrock VPC 엔드포인트, Argo CD. 시연 기간에만 띄운다.

Argo CD는 모니터링, NetworkDoctor, HTTPRoute(`gateway-routes`), Cat Shop을 배포한다.
시연 전에는 **워커 3대에서 수집기가 실행되고, 같은 알림이 백엔드에 기록되며
팀 Slack에 도착하는지** 확인한다. HolmesGPT는 선택 사항이다.

- **EKS 1.35 표준 지원, Cilium 1.19.10, AL2023 x86-64 워커.** 두 가용 영역에
  노드 그룹을 하나씩 두고 온디맨드 `t3a.medium`을 `node_counts`(기본 2대·1대)로
  고정한다. 노드 교체와 계획된 업데이트는 별도이므로 업데이트는 시연 시간 밖에서 진행한다.
- **워커는 프라이빗 서브넷에 둔다.** 워커는 공인 IPv4 없이 내부 EKS API를
  사용하고, NAT Gateway 1개를 통해 Slack, 이미지 레지스트리, Git에 연결한다.
  두 가용 영역이 NAT 하나를 공유하므로 해당 영역의 장애에 아웃바운드가 영향을 받는다.
- **팀은 Gateway API로 만든 ALB에서 HTTPS로 조회한다.** Terraform이 만든
  Gateway를 AWS Load Balancer Controller가 두 퍼블릭 서브넷의 ALB로 만들고,
  HTTPRoute마다 Pod IP를 대상으로 등록한다(NodePort 없음). external-dns가
  `nd.<domain>`, `grafana.<domain>` 레코드를 Route53에 쓴다. **ALB 앞에 로그인이
  없으므로 누구나 접속할 수 있다.** 그래서 NetworkDoctor 라우트는 `GET`/`HEAD`만
  전달하고(조회 전용), 변경 기능은 팀 운영자가 SSO로 접속한 뒤 로컬 포트 전달로 사용한다.
  Grafana는 로그인과 조회에 `POST`를 쓰므로 모든 메서드를 전달하고, Grafana 자체
  로그인으로 보호한다. 관리자 비밀번호는 Terraform이 만든다.
- **EKS API 퍼블릭 엔드포인트는 모든 IP에 열려 있다.** 팀원과 GitHub Actions 러너의
  IP가 고정되어 있지 않기 때문이다. 요청마다 IAM 서명 토큰이 필요하고, 클러스터
  access entry에 등록된 역할(GitHub Actions 역할, `admin_role_arns`, `team_role_arns`)만
  권한을 받는다.
- **Bedrock 호출은 VPC 안에서 끝난다.** `bedrock-runtime` 인터페이스 엔드포인트가
  프라이빗 서브넷에 있고, `llm-gateway/llm-gateway` 서비스 계정이 Pod Identity로
  Claude 모델 호출 권한을 받는다. LLM 게이트웨이 배포는 별도 단계다.
- Cilium은 VPC CNI 위에서 네트워크 정책과 Hubble 메트릭을 제공한다. VPC CNI는
  IRSA, EBS CSI·Load Balancer Controller·external-dns는 Pod Identity로 각자의
  IAM 역할을 사용한다. 일반 Pod의 EC2 메타데이터 접근에는
  IMDSv2 홉 제한을 적용한다. 호스트 네트워크 수집기는 신뢰된 워크로드로 취급한다.
  Prometheus와 백엔드 데이터는 가용 영역에 종속된 EBS에 저장되므로 둘 다 워커가
  2대인 첫 번째 가용 영역에 고정한다. 그 영역의 워커가 모두 없어지면 복구가 지연될 수 있다.

## 준비. 상태 버킷과 GitHub Actions 역할 (한 번만)

`envs/bootstrap`은 dns·eks-lab 상태를 보관할 S3 버킷(버전 관리, S3 잠금 파일)과
GitHub Actions가 OIDC로 맡는 IAM 역할 두 개를 만든다. AWS 키를 저장소에 두지 않는다.

- `networkdoctor-github-actions-plan` — 읽기 전용(`ReadOnlyAccess`, 클러스터 조회).
  저장소 브랜치의 PR과 모든 브랜치의 수동 plan이 쓴다. PR이 워크플로를 고쳐도
  이 역할 이상은 얻지 못한다.
- `networkdoctor-github-actions` — apply·destroy용(`PowerUserAccess`와 `nd-eks-lab-*`
  이름의 IAM 리소스 관리, 클러스터 관리자). **`main` 브랜치에서 실행된 워크플로만**
  맡을 수 있으므로 리뷰를 거쳐 머지된 코드만 적용된다. 저장소 관리자가 `main`에
  브랜치 보호(PR 필수)를 걸어 두어야 이 제한이 의미가 있다.

관리자 권한의 로컬 세션에서 한 번 적용한다.
계정에 GitHub OIDC 공급자가 이미 있으면 `-var create_github_oidc_provider=false`를 붙인다.

```sh
cd deploy/terraform/envs/bootstrap
terraform init
terraform apply
terraform output
```

이 스택의 상태 파일(`terraform.tfstate`)은 로컬에 남는다. 커밋하지 말고 운영자가 보관한다.
저장소 관리자(weeeeestern)가 Settings → Secrets and variables → Actions →
**Variables**에 아래 값을 등록한다. 비밀 값은 아니다.

| 변수 | 값 |
|---|---|
| `AWS_PLAN_ROLE_ARN` | `terraform output -raw github_actions_plan_role_arn` |
| `AWS_APPLY_ROLE_ARN` | `terraform output -raw github_actions_apply_role_arn` |
| `TF_STATE_BUCKET` | `terraform output -raw state_bucket` |
| `TF_ADMIN_ROLE_ARNS` | 로컬에서 kubectl을 쓸 운영자 역할 ARN의 JSON 목록. 예: `["arn:aws:iam::…:role/…/AWSReservedSSO_…"]` |
| `TF_TEAM_ROLE_ARNS` | 선택. `team_role_arns`와 같은 JSON 목록 |

로컬에서 dns·eks-lab을 실행할 때도 같은 버킷을 쓴다. 이후 단계의 `terraform init`은
모두 아래처럼 버킷을 넘긴다.

```sh
state_bucket=$(terraform -chdir=deploy/terraform/envs/bootstrap output -raw state_bucket)
terraform -chdir=deploy/terraform/envs/dns init -backend-config="bucket=$state_bucket"
```

## 0. DNS 스택 (한 번만)

`netdoc.cloud`는 가비아에 등록되어 있고 지금은 가비아 네임서버를 쓴다. 이 단계는
**도메인 전체의 DNS를 Route53으로 옮긴다.** 가비아 DNS에 따로 쓰는 레코드가 있으면
먼저 Route53 영역에 옮겨 둔다. 일부만 위임하려면 `domain`을 `lab.netdoc.cloud`처럼
서브도메인으로 두고, 가비아에 그 호스트의 NS 레코드 4개만 추가한다. 두 스택의
`domain`과 `deploy/helm/gateway-routes/values.yaml`의 `domain`은 같아야 한다.

네임서버 변경은 전파에 수 시간이 걸릴 수 있으므로 **시연 하루 전까지** 마친다.
영역을 먼저 만들고, 네임서버를 바꾼 뒤 인증서 검증까지 적용한다.

GitHub에서는 Actions → terraform → Run workflow에서 브랜치 `main`, `stack=dns`, `action=apply`,
`target=aws_route53_zone.this`로 첫 적용을, 위임 확인 후 `target` 없이 두 번째 적용을
실행한다. 로컬에서는 다음과 같다.

```sh
cd deploy/terraform/envs/dns
terraform init -backend-config="bucket=$state_bucket"
terraform apply -target=aws_route53_zone.this
terraform output name_servers
```

출력된 네 개를 My가비아 → 도메인 관리 → 네임서버 설정에서 "타사 네임서버"로
등록한다. `dig +short NS netdoc.cloud`에 Route53 네임서버가 보이면 나머지를 적용한다.
ACM 검증은 위임이 보일 때까지 최대 3시간 기다린다.

```sh
dig +short NS netdoc.cloud
terraform apply
terraform output certificate_arn
```

이 스택은 랩을 지울 때 함께 지우지 않는다. 영역 요금은 월 $0.50이다.

## 1. 배포 전 확인과 플랫폼 생성

`envs/dns`의 인증서가 `ISSUED`인지 먼저 확인한다. 랩 스택은 `domain`으로
영역과 인증서를 조회하므로, 없으면 계획 단계에서 멈춘다.

AWS CLI SSO 프로필, Terraform 1.5 이상, EKS 1.35와 호환되는 kubectl
1.34–1.36을 준비한다. 생성과 정리를 맡을 운영자 한 명을 정하고,
적용에 사용하는 AWS 계정·리전·Terraform 작업 폴더와 상태 파일을 보관한다.

```sh
export AWS_PROFILE=networkdoctor  # 실제 운영자 SSO 프로필
aws sso login
aws sts get-caller-identity --query '{Account:Account,Role:Arn}' --output table
aws service-quotas get-service-quota --region ap-northeast-2 \
  --service-code ec2 --quota-code L-1216C47A \
  --query 'Quota.{Name:QuotaName,AppliedValue:Value}' --output table
aws ec2 describe-instance-type-offerings --region ap-northeast-2 \
  --location-type availability-zone --filters Name=instance-type,Values=t3a.medium \
  --query 'InstanceTypeOfferings[].Location' --output text
aws acm list-certificates --region ap-northeast-2 \
  --query 'CertificateSummaryList[].{Domain:DomainName,Status:Status}' --output table
```

`availability_zones`의 두 영역이 `t3a.medium` 제공 목록에 있어야 한다.

서울 리전의 **Running On-Demand Standard instances**에 기존 사용분과 용량 예약을
제외하고도 **6 vCPU**가 남아 있어야 한다. 콘솔의 Service Quotas에서 적용 한도와
사용량을 확인한다. 부족하면 한도가 늘어난 후 배포한다. 워커가 생성에 실패해도
이미 만들어진 제어 영역은 과금될 수 있다.
[AWS 온디맨드 할당량](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-on-demand-instances.html#ec2-on-demand-instances-quotas)

저장소 루트에서 시작한다. 랩 스택은 커밋된 변수 기본값으로 실행하며 `terraform.tfvars`가
필요 없다. 예전 `terraform.tfvars`가 있으면 아래 항목을 확인한 뒤 지운다.

- `admin_role_arns`(로컬에서 Terraform이나 kubectl을 실행할 IAM 역할·사용자 ARN)는
  **tfvars에 넣지 않는다.** GitHub Actions는 tfvars를 읽지 못하므로, 로컬에만 있는 값은
  다음 CI apply가 기본값으로 되돌려 그 신원의 access entry를 지운다. 저장소 변수
  `TF_ADMIN_ROLE_ARNS`에 넣고 로컬에서도 같은 값을 환경변수로 넘긴다
  (`export TF_VAR_admin_role_arns='["arn:aws:iam::…"]'`). 클러스터 관리자는 "클러스터를 만든
  주체"가 아니라 GitHub Actions 역할과 이 목록이다. 둘 다 없으면 계획 단계에서 멈춘다.
  같은 이유로 `node_counts`·`domain` 등도 tfvars에서 바꾸지 말고 변수 기본값을 고쳐 커밋한다.
- `domain`은 `envs/dns`와 같은 값으로 둔다.
- `availability_zones`의 첫 영역 노드 그룹에 `networkdoctor.io/stateful=true` 라벨이 붙고,
  Prometheus와 backend가 이 라벨로 그 노드 그룹에 고정된다. 영역을 바꿔도 Helm 값은 그대로다.
- 팀 운영자가 직접 접속해야 하면 `team_role_arns`에 IAM 콘솔의 전체 SSO 역할
  ARN(`arn:aws:iam::…:role/…/AWSReservedSSO_…`)을 넣는다.

예전 입력의 `node_count`, `team_alb_certificate_arn`, `team_api_cidrs`는 제거하고
`node_counts`, `availability_zones`, `domain`, `admin_role_arns`를 새 예시대로 넣는다.

```sh
cd deploy/terraform/envs/eks-lab
export TF_VAR_admin_role_arns='["arn:aws:iam::…"]'  # 저장소 변수 TF_ADMIN_ROLE_ARNS와 같은 값
terraform init -backend-config="bucket=$state_bucket"
terraform plan -out=lab.tfplan
terraform show lab.tfplan
```

GitHub에서는 Run workflow에서 `stack=eks-lab`, `action=plan`으로 계획을 보고(아무 브랜치),
브랜치 `main`에서 `action=apply`로 적용한다. 실행 요약에 계획과 출력이 남는다.
이어지는 Argo CD 등록·동기화와 Secret 등록은 `admin_role_arns`의 역할로 로컬에서 한다.

이 구성은 **신규 시연 클러스터** 기준이다. 계획에서 기존 환경의 교체·삭제가
보이면 적용을 멈추고 상태와 계정을 확인한다. 이전 구성(세 가용 영역, Terraform이
직접 만든 ALB)으로 띄운 랩이 있으면 먼저 그 상태로 정리한 뒤 새로 만든다.
기존 클러스터를 직접 업그레이드할 때는 EKS 버전과 애드온의 전환 순서를 별도로 검토한다.

위 계획을 확인한 후 적용한다. 일부만 생성되고 실패했다면 같은 상태와 입력으로
원인을 해결하고 다시 계획하거나 정리한다. 상태 파일은 Git에 올리지 않는다.

```sh
terraform apply lab.tfplan
aws eks update-kubeconfig --region ap-northeast-2 --name nd-eks-lab
kubectl config current-context
kubectl get nodes
```

리전·클러스터 이름을 바꿨다면 `terraform output -raw configure_kubectl`의
연결 설정을 사용한다. 컨텍스트에 해당 계정·리전·클러스터가 표시되고
워커 **3대가 Ready**인지 확인한다.

## 2. 수집기 확인과 앱 배포

운영자는 세 워커 각각에서 기본 IPv4 경로의 인터페이스를 확인한다. 노드 관리 셸에서
`ip -4 route show default`를 실행하거나, 운영자 권한으로 아래와 같이 확인하고
생성된 디버그 Pod를 제거한다.

```sh
worker_name='실제 워커 이름'
kubectl debug "node/$worker_name" -it --image=busybox:1.37.0 -- ip -4 route show default
```

경로의 `dev` 값이 EKS Helm 기본값인 `ens5`와 일치하는지 확인한다.
다르면 배포할 Git 커밋의 `deploy/helm/networkdoctor/values-eks-lab.yaml`에서
`agent.iface`를 바꾼다. 클러스터에 다른 이름을 붙였다면 같은 파일의
`global.clusterName`도 맞춘다. 수집기와 백엔드 이미지는 검토한 공개 amd64
다이제스트를 기본으로 사용한다. 이미지를 바꿀 때는 해당 풀 권한과
`agent.image.digest`·`backend.image.digest`를 함께 확인한다.

아래 명령은 배포할 커밋을 체크아웃한 상태에서 저장소 루트에서 실행한다.
해당 커밋이 GitHub에 올라 있는지 확인한다. 루트 Application에 그 커밋 SHA를
지정하고 저장소 내부 자식 앱도 같은 커밋을 사용한다. 소스 저장소가 포크라면 루트 Application의
`repoURL`과 `deploy/bootstrap/eks-lab.yaml`의 자식 공통 `source.repoURL`을
둘 다 맞춘다.

```sh
kubectl -n kube-system rollout status daemonset/eks-pod-identity-agent
kubectl -n kube-system rollout status daemonset/aws-node
kubectl -n kube-system rollout restart deployment/coredns
kubectl -n kube-system rollout status deployment/coredns
deploy_revision=$(git rev-parse HEAD)
kubectl patch --local -f deploy/argocd/root-application-eks-lab.yaml --type=merge \
  -p "{\"spec\":{\"source\":{\"targetRevision\":\"$deploy_revision\"}}}" -o yaml |
  kubectl apply -f -
```

운영자 kubeconfig로 Argo CD 동기화를 요청한다. 아래 블록을 **한 줄씩 실행**하고,
각 앱의 `SYNC STATUS`가 `Synced`인지 확인하고 넘어간다. 자식 앱은
`HEALTH STATUS`도 `Healthy`인지 확인한다. `-w` 화면은 확인 후 Ctrl-C로
빠져나온다. [kubectl을 사용한 Argo CD 동기화](https://argo-cd.readthedocs.io/en/stable/user-guide/sync-kubectl/)

```sh
sync_operation='{"operation":{"sync":{"syncStrategy":{"hook":{}}}}}'
kubectl -n argocd patch application networkdoctor-root --type=merge -p "$sync_operation"
kubectl -n argocd get application networkdoctor-root -w
kubectl -n argocd patch application monitoring --type=merge -p "$sync_operation"
kubectl -n argocd get application monitoring -w
kubectl -n argocd patch application networkdoctor --type=merge -p "$sync_operation"
kubectl -n argocd get application networkdoctor -w
kubectl -n argocd patch application gateway-routes --type=merge -p "$sync_operation"
kubectl -n argocd get application gateway-routes -w
kubectl -n argocd patch application networkdoctor-demo --type=merge -p "$sync_operation"
kubectl -n argocd get application networkdoctor-demo -w
```

monitoring의 Prometheus 리소스 유형이 등록되고 Prometheus PVC가 `Bound`인지,
NetworkDoctor의 백엔드 PVC가 `Bound`이며 수집기 DaemonSet이 3/3인지 확인한다.

```sh
kubectl -n monitoring get pvc
kubectl -n networkdoctor get pvc
kubectl -n networkdoctor rollout status daemonset/networkdoctor-agent --timeout=180s
kubectl -n monitoring port-forward svc/monitoring-kube-prometheus-prometheus 19090:9090
```

Prometheus의 `http://localhost:19090/targets`에서 Cat Shop·Cilium·Hubble 수집
대상과 워커별 에이전트 대상이 응답하는지 확인한다.
`count(up{namespace="networkdoctor",service="networkdoctor-agent"} == 1)`의 결과는
`3`이어야 한다. `Pending` PVC가 있으면 `gp2` StorageClass와 EBS CSI 컨트롤러
이벤트를 확인하고 시연은 원인이 해소된 후 진행한다.

HolmesGPT를 쓸 때만 `holmesgpt` 네임스페이스를 먼저 만든다. EKS에서 접근할 수
있는 게이트웨이 URL을 Holmes EKS Helm 값의 `holmes.modelList.<모델>.api_base`에
지정하고 `holmes-llm` Secret의 `LLM_GATEWAY_KEY`를 등록한다. Holmes 앱을
동기화해 준비된 뒤, NetworkDoctor EKS 값의 `backend.config.holmesURL`을
`http://holmesgpt-holmes.holmesgpt.svc:80`으로 바꾸고 다시 동기화한다.
클러스터 안의 LLM 게이트웨이는 `llm-gateway` 네임스페이스의 `llm-gateway`
서비스 계정으로 띄운다. Terraform이 그 계정에 Bedrock Pod Identity를 미리 연결해 둔다.

### 팀 HTTPS 접속

`gateway-routes`를 동기화하면 Load Balancer Controller가 ALB에 라우트를 붙이고
external-dns가 Route53 레코드를 만든다. Gateway가 `Programmed`이고 라우트가
`Accepted`인지 확인한 뒤 접속한다. `GET`은 성공하고, `POST`는
라우트에 일치하지 않아 ALB 기본 응답(4xx)을 받는다(NetworkDoctor 라우트는 `GET`/`HEAD`만 일치).

```sh
kubectl -n gateway get gateway lab
kubectl -n networkdoctor get httproute networkdoctor -o jsonpath='{.status.parents[0].conditions}'
kubectl -n monitoring get httproute grafana
dig +short nd.netdoc.cloud
curl -4 -fsS --max-time 15 https://nd.netdoc.cloud/healthz
curl -4 -sS --max-time 15 -o /dev/null -w '%{http_code}\n' -X POST https://nd.netdoc.cloud/healthz
```

DNS가 아직 없으면 `kubectl -n kube-system logs deploy/external-dns`에서 레코드 생성을
확인한다. 대상이 `unhealthy`이면 EC2 콘솔의 대상 그룹에서 Pod IP와 헬스체크 경로를 본다.

Grafana(`https://grafana.netdoc.cloud`)는 `admin`과 다음 비밀번호로 로그인한다.
GitHub Actions로 적용했다면 같은 버킷으로 로컬 `init` 후 읽거나 Secret에서 읽는다.

```sh
terraform -chdir=deploy/terraform/envs/eks-lab output -raw grafana_admin_password
kubectl -n monitoring get secret grafana-admin -o jsonpath='{.data.admin-password}' | base64 -d
```

### 팀 운영자 SSO·포트 전달

`team_role_arns`에는 신뢰된 팀 운영자의 SSO IAM 역할 ARN만 넣는다.
`admin_role_arns`에 넣은 관리자 역할은 이 목록에서 제외한다. 별도 SSO 역할에
`eks:DescribeCluster`가 필요하다. 배포 후 팀원 접속을 추가하는 경우 같은
Terraform 상태에서 입력을 갱신하고 다시 계획·적용한다. Kubernetes 권한은
`networkdoctor` 내 Pod 포트 전달을 허용한다. 백엔드 변경 기능과 호스트 네트워크
수집기가 같은 범위에 있다. 조회만 하는 팀원은 `https://nd.netdoc.cloud`를 사용한다.

NetworkDoctor 네임스페이스가 생긴 뒤 운영자가 적용한다.

```sh
kubectl apply -f deploy/terraform/k8s/team-access.yaml
```

팀 운영자는 각자 SSO 및 kubeconfig를 연결한다. `kubectl auth whoami`로 역할을
확인한다. 아래 권한 확인의 첫 두 결과는 `yes`, 나머지는 `no`가 예상된다.

```sh
export AWS_PROFILE=networkdoctor-team  # 팀원 자신의 SSO 프로필
aws sso login
aws eks update-kubeconfig --region ap-northeast-2 --name nd-eks-lab
kubectl config current-context
kubectl auth whoami
kubectl -n networkdoctor auth can-i get services/networkdoctor-backend
kubectl -n networkdoctor auth can-i create pods --subresource=portforward
kubectl -n networkdoctor auth can-i get secrets
kubectl -n networkdoctor auth can-i create pods --subresource=exec
kubectl -n kube-system auth can-i create pods --subresource=portforward
kubectl -n networkdoctor port-forward svc/networkdoctor-backend 8080:8080
```

포트 전달을 유지하고 브라우저에서 `http://localhost:8080`을 연다.

## 3. Slack 연결과 시연 확인

팀 시연 채널에 등록한 Slack Incoming Webhook URL을
`networkdoctor/networkdoctor-slack` Secret의 `webhook-url` 키로 넣는다.
실제 URL은 Git이나 Terraform 변수에 넣지 않는다. 현재 컨텍스트가 시연 EKS인지
확인하고 저장소 루트에서 실행한다.

```sh
printf 'Slack Incoming Webhook URL: '
read -rs nd_slack_webhook
printf '\n'
if [ -n "$nd_slack_webhook" ]; then
  printf '%s' "$nd_slack_webhook" |
    kubectl -n networkdoctor create secret generic networkdoctor-slack \
      --from-file=webhook-url=/dev/stdin --dry-run=client -o yaml |
    kubectl apply --server-side -f - &&
    kubectl apply -f deploy/terraform/k8s/alertmanager-slack.example.yaml
fi
unset nd_slack_webhook
```

[EKS 알림 전달 리허설](k8s/eks-alert-smoke.md)을 실행한다. 4분 후 스스로 만료되는
테스트 알림 하나로 **Prometheus 발화 → 백엔드 Incident → 팀 Slack의 발화·복구**를
같은 실행 ID로 확인한다. HolmesGPT 연결은 이 시험의 조건이 아니다.
장애 자체를 보여주는 시연과 수집기 상태 검증은 따로 진행한다.

## 4. 5일 뒤 정리

120시간 운영 시 기본 비용은 **약 $45**로 추정한다. 기존 약 $32 구성에서
NAT Gateway 1개에 $7.08, ALB 1개에 $2.70, Bedrock 인터페이스 엔드포인트
2개(가용 영역별)에 약 $3.1이 추가된다. 공인 IPv4는 NAT용 1개와 ALB의 가용 영역별
2개로 3개다. NAT 데이터 처리($0.059/GB), ALB 사용량($0.008/LCU-시간), 엔드포인트
데이터 처리($0.01/GB), 가용 영역 간 전송, 로그, 초과 CPU 크레딧, Bedrock 토큰
사용량과 세금은 별도다. `envs/dns`의 Route53 영역(월 $0.50)은 랩과 별개로 계속 과금된다.
운영 시작 전에 종료 시각과 담당자를 정한다. 종료는 수동이다.
[AWS EKS](https://aws.amazon.com/eks/pricing/) · [EC2](https://aws.amazon.com/ec2/pricing/on-demand/) ·
[VPC](https://aws.amazon.com/vpc/pricing/) · [Elastic Load Balancing](https://aws.amazon.com/elasticloadbalancing/pricing/) ·
[EBS](https://aws.amazon.com/ebs/pricing/) · [KMS](https://aws.amazon.com/kms/pricing/)

1. 배포 때와 같은 AWS 계정·리전·Terraform 상태인지 확인한다.
   백엔드에서 보관할 보고서를 내려받는다. 백엔드 DB나 Prometheus 기록도
   필요하면 해당 데이터 볼륨을 백업하고 완료를 확인한 뒤 앱을 지운다.
2. `kubectl -n networkdoctor get pvc`와 `kubectl -n monitoring get pvc`로 이 랩의
   claim과 PV를 기록한다. `kubectl get pv <pv-name> -o yaml`에서
   `spec.csi.volumeHandle` 또는 `spec.awsElasticBlockStore.volumeID`의 EBS 볼륨 ID와
   `spec.persistentVolumeReclaimPolicy`를 기록한다. 종료 확인에 이 ID를 쓴다.
3. 임시 알림 규칙과 Slack 라우트를 정리한 뒤 루트 앱과 의존 앱을 지운다.
   루트만 지우면 자식 앱은 남는다. monitoring은 다른 앱이 사용하는 리소스 유형을
   설치하므로 마지막에 지운다. 아래 명령은 저장소 루트에서, 확인한 컨텍스트에 실행한다.

```sh
kubectl -n networkdoctor delete prometheusrule networkdoctor-eks-alert-smoke --ignore-not-found &&
kubectl -n networkdoctor delete alertmanagerconfig networkdoctor-slack --ignore-not-found &&
kubectl -n argocd delete application networkdoctor-root --ignore-not-found &&
kubectl -n argocd delete application gateway-routes networkdoctor-demo networkdoctor holmesgpt --ignore-not-found --timeout=5m &&
kubectl -n argocd delete application monitoring --ignore-not-found --timeout=5m
```

monitoring 설치 전에 중단한 랩은 등록되지 않은 리소스 유형의 삭제만 건너뛴다.
삭제가 실패하거나 시간 초과되면 남아 있는 Application과 리소스를 확인한 뒤 계속한다.

4. **워커와 EBS CSI가 살아 있을 때** 남은 랩 전용 PVC/PV를 확인하고, 백업이
   끝난 claim만 삭제한다. `Retain` 정책이나 정리 실패로 볼륨이 남을 수 있다.
   EC2의 볼륨 화면에서 기록한 각 ID를 확인한다. 여전히 남은 볼륨은 백업이
   끝났고 이 랩 전용이며 분리된 상태인지 확인한 뒤 그 ID만 삭제한다.
5. 같은 상태에서 종료 계획을 만든다. 이 랩의 Gateway, NAT Gateway와 NAT용
   탄력적 IP(EIP), Bedrock 엔드포인트를 포함해 랩에서 만든 리소스만 지워지는지
   확인한다. ALB와 대상 그룹은 Terraform 상태에 없다. Terraform이 Gateway를
   먼저 지우고, Load Balancer Controller가 ALB를 지울 때까지 기다린 뒤 컨트롤러와
   VPC를 지운다. `envs/dns`는 계획 대상이 아니다. GitHub에서는 Run workflow에서
   `stack=eks-lab`, `action=destroy`로 계획 확인과 적용을 한 번에 실행한다.

```sh
terraform -chdir=deploy/terraform/envs/eks-lab plan -destroy -out=lab-destroy.tfplan
terraform -chdir=deploy/terraform/envs/eks-lab show lab-destroy.tfplan
```

6. 종료 계획이 맞으면 적용한다. 서울 리전 콘솔에서 랩의 ALB, NAT Gateway, EIP,
   VPC 엔드포인트, EKS, EC2, EBS 삭제가 끝났는지 확인한다. 리소스가 삭제 중이면
   완료될 때까지 확인한다. 3단계에서 `gateway-routes`를 지웠다면 external-dns가
   `nd.`·`grafana.` 레코드를 이미 지웠다. Route53 영역에 남은 랩 레코드가 있으면
   지운다(영역과 ACM 검증 레코드는 유지). 보존하기로 한 스냅샷이나 로그에는 비용이 이어질 수 있다.

```sh
terraform -chdir=deploy/terraform/envs/eks-lab apply lab-destroy.tfplan
```
