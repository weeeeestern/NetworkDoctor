# NetworkDoctor — AWS EKS lab (Terraform)

온프렘 랩과 동일한 스택을 AWS에서 돌리기 위한 환경. 역할 분담:

- **Terraform (`envs/eks-lab`)** — 플랫폼 레이어: VPC → EKS → Cilium →
  AWS Load Balancer Controller → Route53/ACM → external-dns → **Argo CD**
- **Argo CD (`deploy/bootstrap/eks-lab.yaml`)** — 앱 레이어: 온프렘과 달리
  **kube-prometheus-stack(Prometheus·Alertmanager·Grafana)도 Argo 애플리케이션**
  으로 올라간다. networkdoctor, networkdoctor-demo, holmesgpt도 동일.

외부 접근은 **Kubernetes Gateway API**(Ingress 후속 표준)로 구성한다.

```
사용자 ──https://netdoc.cloud──> ALB (internet-facing)
            ↑ Route53 (external-dns가 자동 등록)      ↑ ALB controller가
            ↑ 가비아는 NS 위임만                        Gateway/HTTPRoute로 생성
                                  └─ HTTPRoute ──> networkdoctor-backend :8080
```

- 외부 접근 경로는 Terraform이 아니라 **k8s 매니페스트**(`k8s/gateway.yaml`:
  GatewayClass + Gateway + HTTPRoute)가 정의한다. ALB controller v2.13+가
  Gateway 리소스로부터 internet-facing ALB를 만들어 준다 (feature gate
  `ALBGatewayAPI`는 Terraform이 켜 둠).
- 도메인 `netdoc.cloud`는 가비아에 등록돼 있고, DNS만 Route53 존으로
  위임한다. ACM 인증서(apex + `*.netdoc.cloud`)는 DNS 검증으로 자동 발급.
- **HTTPS 인증서는 Gateway의 `tls.certificateRefs`가 아니라
  `LoadBalancerConfiguration.listenerConfigurations.defaultCertificate`로
  연결한다** (ALB controller가 certificateRefs 미지원; tls 블록 생략이 정석).
- external-dns가 HTTPRoute의 `hostnames`를 보고 Route53 A(ALIAS) 레코드를
  자동 생성/삭제한다. ALB가 재생성돼도 레코드가 따라간다.
- Gateway API는 온프렘 랩의 Cilium도 구현하므로, 같은 HTTPRoute 매니페스트를
  온프렘에서도 재사용할 수 있다 (GatewayClass만 환경별로 다름).
- CNI는 **VPC CNI + Cilium chaining**(`cilium.tf`). IPAM/라우팅은 VPC CNI가
  유지하고(ALB ip 타겟 경로 보존), Cilium은 그 위에서 NetworkPolicy 집행과
  Hubble 관측을 담당한다. 온프렘과 동일하게 agent 메트릭(:9962, `cilium_*`)과
  Hubble 메트릭(:9965, `hubble_*`, `[dns, drop, tcp, flow]`)을 켜 두었으므로
  **Rule 1–8 전부 EKS에서 동작**한다.
- Cilium보다 먼저 뜬 파드는 Cilium 관리 밖이다. ALB controller와
  external-dns는 Terraform이 Cilium 뒤에 설치하지만, **CoreDNS는 클러스터와
  함께 뜨므로 최초 1회 재시작이 필요**하다 (아래 3.5단계).
- 백엔드 PVC용으로 EBS CSI 드라이버 addon이 들어간다. 차트의
  `storageClassName`은 `gp2`(EKS 기본) 또는 gp3 StorageClass를 만들어 지정.

## Apply

```sh
cd deploy/terraform/envs/eks-lab
cp terraform.tfvars.example terraform.tfvars   # 필요하면 수정
terraform init
terraform apply        # 15–20분 (EKS 클러스터 생성이 대부분)
```

apply 중 ACM 검증 단계에서 멈춰 있으면 정상이다 — 아래 1번(가비아 NS 위임)이
끝나야 검증이 통과된다 (최대 75분 대기 후 타임아웃).

## Apply 후 순서

```sh
# 1. 가비아 NS 위임 (최초 1회): 아래 4개를
#    My가비아 → 도메인 관리 → 네임서버 설정 → "타사 네임서버"에 입력
terraform output route53_name_servers

# 2. kubeconfig
aws eks update-kubeconfig --region ap-northeast-2 --name nd-eks-lab

# 3. Gateway API CRD (standard channel) — ALB controller/external-dns의 전제
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml

# 3.5. Cilium보다 먼저 뜬 파드를 Cilium 관리 하에 재기동 (최초 1회)
#      — 클러스터와 함께 뜨는 비-hostNetwork 파드: CoreDNS, EBS CSI controller
kubectl -n kube-system rollout restart deployment coredns ebs-csi-controller

# 4. 앱 배포는 전부 Argo CD로 — root app 등록 후 UI에서 sync
#    (Argo는 GitHub main을 보므로, 이 변경들이 main에 머지돼 있어야 한다)
kubectl apply -f deploy/argocd/root-application-eks-lab.yaml

#    UI 접속 + admin 비밀번호:
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
kubectl -n argocd port-forward svc/argocd-server 8443:443
#    sync 순서: networkdoctor-root → monitoring(Operator CRD 먼저!)
#    → networkdoctor → networkdoctor-demo → (holmesgpt는 Secret 준비 후)

# 5. 인증서 ARN 채워서 Gateway + HTTPRoute 적용 → ALB 생성 (2–3분)
CERT_ARN=$(terraform -chdir=deploy/terraform/envs/eks-lab output -raw acm_certificate_arn)
sed "s|ACM_CERT_ARN|$CERT_ARN|" deploy/terraform/k8s/gateway.yaml | kubectl apply -f -

# 6. 확인 — external-dns가 netdoc.cloud → ALB 레코드를 만들 때까지 1–2분
kubectl -n networkdoctor get gateway networkdoctor
curl https://netdoc.cloud/healthz

# 7. Grafana (admin 비밀번호 조회 후 port-forward로 접속)
kubectl -n monitoring get secret monitoring-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80
```

HTTPRoute에 `hostnames: [netdoc.cloud]`가 있어서 ALB 기본 DNS 주소로의
요청은 404가 난다 — 항상 도메인으로 접근할 것.

## 비용 메모

상시 과금: EKS control plane($0.10/h) + NAT gateway 1개 + t3.large 노드 +
ALB. **실험 없는 날은 `terraform destroy`가 제일 싸다** — 전부 코드라서
다시 올리면 된다. destroy 전에 `kubectl delete -f deploy/terraform/k8s/gateway.yaml`로
Gateway를 먼저 지워야 컨트롤러가 만든 ALB가 남지 않는다 (Route53 레코드는
external-dns가 정리). Route53 존은 월 $0.5, 도메인은 가비아 연 단위 결제.
state는 S3 백엔드 설정(`backend.tf` 주석) 후 보관할 것.

## 파일

| 파일 | 내용 |
| --- | --- |
| `envs/eks-lab/main.tf` | VPC + EKS(+addons, Pod Identity) |
| `envs/eks-lab/albc.tf` | AWS Load Balancer Controller v2.13 (IAM + Helm, Gateway API feature gate) |
| `envs/eks-lab/cilium.tf` | Cilium 1.18 chaining 모드 (NetworkPolicy + Hubble 메트릭, Rule 8/3용) |
| `envs/eks-lab/argocd.tf` | Argo CD 설치 (앱 배포는 전부 Argo가 담당) |
| `../bootstrap/eks-lab.yaml` | Argo 자식 앱 정의: monitoring(kps)·networkdoctor·demo·holmesgpt |
| `../argocd/root-application-eks-lab.yaml` | 수동 등록하는 root app (app-of-apps) |
| `envs/eks-lab/dns.tf` | Route53 존 + ACM 와일드카드 인증서(DNS 검증) |
| `envs/eks-lab/external-dns.tf` | external-dns (HTTPRoute hostname → Route53 자동 등록) |
| `k8s/gateway.yaml` | GatewayClass·LoadBalancerConfiguration·Gateway·HTTPRoute |
