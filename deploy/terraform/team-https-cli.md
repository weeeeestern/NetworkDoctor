# 팀 시연 주소를 HTTPS로 연결하기

서울 리전 EKS 시연 주소는 `demo.netdoc.cloud`를 사용한다. 팀원이 등록한
`netdoc.cloud` 아래의 이름이다. 루트 도메인에는 기존 A 레코드가 있고, 루트에는
표준 CNAME을 등록할 수 없다. 루트 연결 변경은 도메인 소유자와 별도로 결정한다.
[DNS 레코드 종류](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/ResourceRecordTypes.html)

공개 조회에서 `netdoc.cloud`는 가비아 네임서버를 사용한다. 아래 순서 중
**ACM 검증 레코드와 ALB 연결 레코드**만 가비아 DNS에 추가한다. 팀원에게 두 건을
등록해 달라고 하거나 [DNS 편집 권한](https://customer.gabia.com/manual/domain/287/12440)을
받는다. 가비아의 공식 등록 경로는 웹 DNS 관리툴이다. 다음 명령은 저장소 루트의
같은 터미널에서 실행한다. [가비아 DNS 설정](https://customer.gabia.com/faq/detail/3041/3040)

```sh
demo_hostname=demo.netdoc.cloud
dig +short NS netdoc.cloud
dig +short A netdoc.cloud
dig "$demo_hostname" A +noall +comments +answer
dig "$demo_hostname" CNAME +noall +comments +answer
```

시연 주소에 기존 응답이 있으면 팀원에게 사용 중인지 확인한다.

## 1. AWS CLI로 서울 리전 ACM 인증서 발급

AWS CLI v2로 ALB를 만들 계정에 로그인한다. SSO 프로필이 처음이라면
`aws configure sso --profile networkdoctor --use-device-code --no-browser`를 실행해
팀의 IAM Identity Center 시작 URL과 리전을 입력한다. ACM과 EKS 리전은
`ap-northeast-2`다. 아래 로그인 명령은 URL과 코드를 출력하며, SSO 승인만
브라우저에서 한다. 다른 기기에서 승인해도 된다.
[AWS CLI SSO](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sso.html)

```sh
export AWS_PROFILE=networkdoctor
aws sso login --use-device-code --no-browser
aws sts get-caller-identity --query '{Account:Account,Role:Arn}' --output table
aws acm list-certificates --region ap-northeast-2 --output table
```

이 계정과 리전에 시연 주소를 포함하는 `ISSUED` 인증서가 있다면 그 ARN을
사용하고 2단계로 간다. 이미 요청한 인증서가 대기 중이면 그 ARN을 `cert_arn`에
넣고 `describe-certificate`부터 실행한다. 새 인증서가 필요할 때만
`request-certificate`를 실행한다.

```sh
cert_arn=$(aws acm request-certificate --region ap-northeast-2 \
  --domain-name "$demo_hostname" --validation-method DNS \
  --query CertificateArn --output text)
printf '%s\n' "$cert_arn"
aws acm describe-certificate --region ap-northeast-2 --certificate-arn "$cert_arn" \
  --query 'Certificate.DomainValidationOptions[0].ResourceRecord' --output json
```

결과의 `Name`과 `Value`가 표시될 때까지 `describe-certificate`를 다시 조회한다.
가비아의 `netdoc.cloud` DNS 관리툴에 **검증용 CNAME**을 추가한다. 예를 들어 ACM의
`Name`이 `_abc.demo.netdoc.cloud.`이면 호스트에는 `_abc.demo`를 입력한다.
값에는 ACM의 `Value`를 끝의 점까지 그대로 넣는다.
[ACM DNS 검증](https://docs.aws.amazon.com/acm/latest/userguide/dns-validation.html)

```sh
validation_name=$(aws acm describe-certificate --region ap-northeast-2 \
  --certificate-arn "$cert_arn" \
  --query 'Certificate.DomainValidationOptions[0].ResourceRecord.Name' --output text)
dig +short CNAME "$validation_name"
aws acm wait certificate-validated --region ap-northeast-2 --certificate-arn "$cert_arn"
aws acm describe-certificate --region ap-northeast-2 --certificate-arn "$cert_arn" \
  --query 'Certificate.Status' --output text
```

`dig`에 ACM이 제공한 값이 표시되고 상태가 `ISSUED`가 되면 진행한다. 대기가
시간 초과되면 CNAME을 확인하고 같은 ARN으로 다시 기다린다.

## 2. Terraform과 Argo CD로 배포

`deploy/terraform/envs/eks-lab/terraform.tfvars`의
`team_alb_certificate_arn`에 발급된 ARN을 넣고, `team_api_cidrs`에 시연에
사용할 팀 공인 IPv4를 `/32`로 넣는다. [배포 런북](README.md#1-배포-전-확인과-플랫폼-생성)의
vCPU 확인 및 Terraform 계획·적용을 진행한 뒤 [CLI 앱 동기화](README.md#2-수집기-확인과-앱-배포)를
실행한다. ALB와 NAT 과금은 생성 시점부터 시작한다.

## 3. ALB 상태 확인 후 시연 주소 연결

NetworkDoctor를 동기화한 뒤 대상 그룹에 `healthy`인 워커가 표시되는지 확인한다.
허용된 팀 IPv4에서 `--connect-to`로 먼저 HTTPS를 시험하면 DNS 레코드가
반영되기 전에도 실제 시연 주소의 인증서를 검증할 수 있다.

```sh
lab_tf_dir=deploy/terraform/envs/eks-lab
target_group_arn=$(terraform -chdir="$lab_tf_dir" output -raw team_alb_target_group_arn)
alb_dns=$(terraform -chdir="$lab_tf_dir" output -raw team_alb_dns_name)
aws elbv2 describe-target-health --region ap-northeast-2 \
  --target-group-arn "$target_group_arn" \
  --query 'TargetHealthDescriptions[].{Worker:Target.Id,Port:Target.Port,State:TargetHealth.State,Reason:TargetHealth.Reason}' \
  --output table
curl -4 -fsS --max-time 15 --connect-to "$demo_hostname:443:$alb_dns:443" \
  "https://$demo_hostname/healthz"
printf 'ALB CNAME 값: %s.\n' "${alb_dns%.}"
```

가비아에 **ALB 연결용 CNAME**을 별도로 추가한다. 호스트는 `demo`, 값은 위에
표시된 ALB 이름이다. 그 뒤 다음을 실행한다. `GET`은 성공, `POST`는 `403`이
예상된다.

```sh
dig +short CNAME "$demo_hostname"
curl -4 -fsS --max-time 15 "https://$demo_hostname/healthz"
curl -4 -sS --max-time 15 -o /dev/null -w '%{http_code}\n' \
  -X POST "https://$demo_hostname/healthz"
```

시연이 끝나면 ALB 연결용 `demo` CNAME을 제거한다. ACM 인증서를 재사용하거나
갱신할 계획이면 검증용 CNAME을 유지한다. [ACM DNS 갱신](https://docs.aws.amazon.com/acm/latest/userguide/dns-renewal-validation.html)
