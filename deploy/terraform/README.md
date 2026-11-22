# NetworkDoctor — AWS EKS 시연 환경

서울 리전에 5일짜리 팀 시연 환경을 만든다. Terraform은 VPC, EKS, NAT Gateway,
ALB, Cilium, Argo CD를 구성한다. Argo CD는 모니터링, NetworkDoctor, Cat Shop을 배포한다.
시연 전에는 **워커 3대에서 수집기가 실행되고, 같은 알림이 백엔드에 기록되며
팀 Slack에 도착하는지** 확인한다. HolmesGPT는 선택 사항이다.

- **EKS 1.35 표준 지원, Cilium 1.19.10, AL2023 x86-64 워커.** 기본 용량은
  온디맨드 `t3a.medium` 3대다. 최소·목표·최대는 모두 `node_count`로 고정한다.
  노드 교체와 계획된 업데이트는 별도이므로 업데이트는 시연 시간 밖에서 진행한다.
- **워커는 프라이빗 서브넷에 둔다.** 워커는 공인 IPv4 없이 내부 EKS API를
  사용하고, NAT Gateway 1개를 통해 Slack, 이미지 레지스트리, Git에 연결한다.
  세 가용 영역이 NAT 하나를 공유하므로 해당 영역의 장애에 아웃바운드가 영향을 받는다.
- **팀은 ALB에서 HTTPS로 조회한다.** ALB는 세 퍼블릭 서브넷에 배치되며, 팀이
  등록한 공인 IPv4에서만 443 포트로 접근할 수 있다. ALB는 `GET`/`HEAD`를
  백엔드로 전달한다. 변경 기능은 팀 운영자가 SSO로 접속한 뒤 로컬 포트 전달로
  사용한다. 팀원이 다른 네트워크로 이동하면 허용 IP를 갱신한다.
- Cilium은 VPC CNI 위에서 네트워크 정책과 Hubble 메트릭을 제공한다. VPC CNI는
  IRSA, EBS CSI는 Pod Identity로 각자의 IAM 역할을 사용한다. 일반 Pod의 EC2 메타데이터 접근에는
  IMDSv2 홉 제한을 적용한다. 호스트 네트워크 수집기는 신뢰된 워크로드로 취급한다.
  Prometheus와 백엔드 데이터는 가용 영역에 종속된 EBS에 저장되므로, 그 영역에
  사용 가능한 워커가 없으면 복구가 지연될 수 있다.

## 1. 배포 전 확인과 플랫폼 생성

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
```

서울 리전의 **Running On-Demand Standard instances**에 기존 사용분과 용량 예약을
제외하고도 **6 vCPU**가 남아 있어야 한다. 콘솔의 Service Quotas에서 적용 한도와
사용량을 확인한다. 부족하면 한도가 늘어난 후 배포한다. 워커가 생성에 실패해도
이미 만들어진 제어 영역은 과금될 수 있다.
[AWS 온디맨드 할당량](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-on-demand-instances.html#ec2-on-demand-instances-quotas)

저장소 루트에서 시작한다. 기존 `terraform.tfvars`는 보존하고 직접 수정한다.

- `team_api_cidrs`에 Terraform 실행 컴퓨터와 팀원이 사용하는 실제 공인 IPv4를
  각각 `/32`로 넣는다. 이 목록은 EKS API와 ALB에 함께 적용된다. 팀이 관리하는
  접속망의 주소를 사용한다.
- `team_alb_certificate_arn`에 서울 리전에서 발급 완료된 ACM 인증서 ARN을 넣는다.
  인증서는 팀이 관리하는 시연 서브도메인(예: `demo.example.com`)과 일치해야 한다.
  시연 DNS 레코드는 앱 배포 후 기존 DNS 서비스에서 추가한다.
- 팀 운영자가 직접 접속해야 하면 `team_role_arns`에 IAM 콘솔의 전체 SSO 역할
  ARN(`arn:aws:iam::…:role/…/AWSReservedSSO_…`)을 넣는다.

예전 입력의 `node_min`, `node_desired`, `node_max`, `domain`은 제거하고
새 예시의 버전과 인스턴스 크기를 확인한다.

```sh
cd deploy/terraform/envs/eks-lab
if [ ! -e terraform.tfvars ]; then
  cp terraform.tfvars.example terraform.tfvars
fi
```

입력값을 확인한 후 계획을 만든다.

```sh
terraform init
terraform plan -out=lab.tfplan
terraform show lab.tfplan
```

이 구성은 **신규 시연 클러스터** 기준이다. 계획에서 기존 환경의 교체·삭제가
보이면 적용을 멈추고 상태와 계정을 확인한다. 이전 구성에서 만든 Gateway와 ALB는
이전 컨트롤러가 살아 있을 때 별도로 정리한다. 기존 클러스터를 직접 업그레이드할
때는 EKS 버전과 애드온의 전환 순서를 별도로 검토한다.

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

아래 명령은 저장소 루트에서 실행한다. 배포할 변경을 GitHub에 커밋과 브랜치로
올린 뒤, 루트 Application의 `spec.source.targetRevision`을 Argo CD가 읽을 수 있는
그 커밋 SHA나 브랜치로 설정한다. 소스 저장소가 포크라면 루트 Application의
`repoURL`과 `deploy/bootstrap/eks-lab.yaml`의 자식 공통 `source.repoURL`을
둘 다 맞춘다. 저장소 내부 자식 앱은 루트가 해석한 커밋을 사용한다.

```sh
kubectl -n kube-system rollout status daemonset/eks-pod-identity-agent
kubectl -n kube-system rollout status daemonset/aws-node
kubectl -n kube-system rollout restart deployment/coredns
kubectl -n kube-system rollout status deployment/coredns
kubectl apply -f deploy/argocd/root-application-eks-lab.yaml
kubectl -n argocd port-forward svc/argocd-server 8443:443
```

포트 전달을 유지하고 브라우저에서 `https://localhost:8443`을 연다. 처음 설치한
Argo CD의 기본 사용자명은 `admin`이다. 별도 터미널에서 운영자가 초기 암호를
확인한다.

```sh
# 암호는 운영자 터미널에서만 확인하고 공유하지 않는다.
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d
```

자체 서명 인증서 때문에 접속하기 어렵다면 운영자 kubeconfig와 Argo CD CLI로
`argocd admin dashboard -n argocd`를 실행해 로컬 대시보드를 열 수 있다.
[Argo CD 로컬 대시보드](https://argo-cd.readthedocs.io/en/stable/operator-manual/core/#using)

루트 앱을 동기화한 뒤 **monitoring → networkdoctor → networkdoctor-demo**
순서로 적용한다. 각 앱의 `Synced`·`Healthy`를 확인하고 다음으로 넘어간다.
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

### 팀 HTTPS 접속

NetworkDoctor가 동기화되면 EKS Helm 설정이 ALB가 연결할 백엔드 NodePort를
준비한다. AWS 콘솔의 대상 그룹에서 `healthy` 상태를 확인한다. 저장소 루트에서
아래 명령으로 ALB의 DNS 이름을 확인한다.

```sh
terraform -chdir=deploy/terraform/envs/eks-lab output -raw team_alb_dns_name
```

팀이 사용하는 기존 DNS 서비스에서 인증서에 포함된 시연 서브도메인의 CNAME을
위 ALB 주소로 연결한다. 도메인의 네임서버 위임은 기존대로 유지한다. 허용된
IP에서 `https://`로 그 서브도메인을 열고, `/healthz`의 정상 응답과 웹 화면을
확인한다. 아래의 `POST` 확인은 `403`이 예상된다.

```sh
demo_hostname='demo.example.com'  # 실제 인증서에 포함된 시연 서브도메인
curl -fsS "https://$demo_hostname/healthz"
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "https://$demo_hostname/healthz"
```

### 팀 운영자 SSO·포트 전달

`team_role_arns`에는 신뢰된 팀 운영자의 SSO IAM 역할 ARN만 넣는다.
클러스터 생성 관리자 역할은 이 목록에서 제외한다. 별도 SSO 역할에
`eks:DescribeCluster`가 필요하다. 배포 후 팀원 접속을 추가하는 경우 같은
Terraform 상태에서 입력을 갱신하고 다시 계획·적용한다. Kubernetes 권한은
`networkdoctor` 내 Pod 포트 전달을 허용한다. 백엔드 변경 기능과 호스트 네트워크
수집기가 같은 범위에 있다. 조회만 하는 팀원은 ALB 주소를 사용한다.

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

120시간 운영 시 기본 비용은 **약 $42**로 추정한다. 기존 약 $32 구성에서
NAT Gateway 1개에 $7.08, ALB 1개에 $2.70, 공인 IPv4가 3개에서 4개로
바뀌면서 $0.60이 추가된다. 네 개는 NAT용 1개와 ALB의 가용 영역별 기본 3개다.
NAT 데이터 처리($0.059/GB), ALB 사용량($0.008/LCU-시간), 가용 영역 간 전송,
로그, 초과 CPU 크레딧, AI 사용과 세금은 별도다. 운영 시작 전에 종료 시각과
담당자를 정한다. 종료는 수동이다.
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
kubectl -n argocd delete application networkdoctor-demo networkdoctor holmesgpt --ignore-not-found --timeout=5m &&
kubectl -n argocd delete application monitoring --ignore-not-found --timeout=5m
```

monitoring 설치 전에 중단한 랩은 등록되지 않은 리소스 유형의 삭제만 건너뛴다.
삭제가 실패하거나 시간 초과되면 남아 있는 Application과 리소스를 확인한 뒤 계속한다.

4. **워커와 EBS CSI가 살아 있을 때** 남은 랩 전용 PVC/PV를 확인하고, 백업이
   끝난 claim만 삭제한다. `Retain` 정책이나 정리 실패로 볼륨이 남을 수 있다.
   EC2의 볼륨 화면에서 기록한 각 ID를 확인한다. 여전히 남은 볼륨은 백업이
   끝났고 이 랩 전용이며 분리된 상태인지 확인한 뒤 그 ID만 삭제한다.
5. 같은 상태에서 종료 계획을 만든다. 이 랩의 ALB, 대상 그룹, NAT Gateway와
   NAT용 탄력적 IP(EIP)를 포함해 랩에서 만든 리소스만 지워지는지 확인한다.

```sh
terraform -chdir=deploy/terraform/envs/eks-lab plan -destroy -out=lab-destroy.tfplan
terraform -chdir=deploy/terraform/envs/eks-lab show lab-destroy.tfplan
```

6. 종료 계획이 맞으면 적용한다. 서울 리전 콘솔에서 랩의 ALB, NAT Gateway, EIP,
   EKS, EC2, EBS 삭제가 끝났는지 확인한다. 리소스가 삭제 중이면 완료될 때까지
   확인한다. 기존 DNS 서비스에 추가한 시연용 CNAME도 제거한다. 보존하기로 한
   스냅샷이나 로그에는 비용이 이어질 수 있다.

```sh
terraform -chdir=deploy/terraform/envs/eks-lab apply lab-destroy.tfplan
```
