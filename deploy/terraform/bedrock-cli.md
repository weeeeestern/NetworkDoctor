# 서울 리전 Bedrock과 HolmesGPT 확인

전시용 HolmesGPT는 AWS 서울 리전(`ap-northeast-2`)의 Bedrock API에
Claude Haiku 4.5 Global 추론 프로필로 요청한다. 서울은 **호출을 보내는
리전**이고 모델 처리는 다른 AWS 리전에서도 이뤄질 수 있다. EKS의
HolmesGPT는 Pod Identity로 모델을 호출한다.
[Claude Haiku 4.5 지원 리전](https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-claude-haiku-4-5.html) · [Global 추론 프로필](https://docs.aws.amazon.com/bedrock/latest/userguide/global-cross-region-inference.html)

## 1. AWS 계정과 모델 조회

AWS CLI v2의 최신 버전을 사용한다. 아래 명령은 같은 터미널에서 실행한다.
`networkdoctor`는 예시 SSO 프로필 이름이다. 실제 배포에 사용할 계정 번호와
역할이 출력되는지 확인한다. 프로필을 처음 만들 때는
`aws configure sso --profile networkdoctor --use-device-code --no-browser`를 실행한다.

```sh
export AWS_PROFILE=networkdoctor
bedrock_region=ap-northeast-2
foundation_id=anthropic.claude-haiku-4-5-20251001-v1:0
inference_profile_id="global.$foundation_id"

aws sso login --use-device-code --no-browser
aws sts get-caller-identity --query '{Account:Account,Role:Arn}' --output table

aws bedrock get-foundation-model-availability --region "$bedrock_region" \
  --model-id "$foundation_id" --output json
aws bedrock get-inference-profile --region "$bedrock_region" \
  --inference-profile-identifier "$inference_profile_id" \
  --query '{Status:status,Profile:inferenceProfileId}' --output table
```

모델 조회의 `authorizationStatus`와 `agreementAvailability`로 권한과 계약
상태를 각각 확인한다. 프로필 상태는 `ACTIVE`가 예상된다. 서울에서 모델을
호출할 때는 `global.`이 붙은 프로필 ID를 사용한다. 조회 결과와 별도로
3단계에서 실제 응답을 확인한다.
[모델 액세스 상태](https://docs.aws.amazon.com/cli/latest/reference/bedrock/get-foundation-model-availability.html) · [추론 프로필 상태](https://docs.aws.amazon.com/cli/latest/reference/bedrock/get-inference-profile.html)

## 2. 최초 이용 목적과 쿼터 확인

이 AWS 계정에서 Anthropic 최초 이용 목적을 제출했는지 다음 명령으로 확인한다.
권한 오류가 나면 현재 AWS 역할의 조회 권한을 확인한다.

```sh
aws bedrock get-use-case-for-model-access --region "$bedrock_region" \
  --query 'formData != null' --output text
```

최초 이용 목적을 제출해야 하면 계정 소유자가 아래 예시를 실제 전시 용도에
맞게 확인한 뒤 실행한다. `intendedUsers`는 팀 내부만 `"0"`, 외부만 `"1"`,
둘 다 `"2"`다. 첫 모델 호출 시 AWS Marketplace 활성화가 진행될 수 있으며
모델 이용약관 동의가 수반된다. 계정 소유자가 약관과 결제 수단을 확인하고
첫 모델 호출을 진행한다.
[AWS 모델 이용 목적 제출과 활성화](https://docs.aws.amazon.com/bedrock/latest/userguide/model-access.html)

```sh
bedrock_form_file=$(mktemp)
cat > "$bedrock_form_file" <<'JSON'
{
  "companyName": "NetworkDoctor graduation project",
  "companyWebsite": "https://github.com/weeeeestern/NetworkDoctor",
  "intendedUsers": "0",
  "industryOption": "Education",
  "otherIndustryOption": "",
  "useCases": "Demonstrate AI-assisted incident diagnosis for a Kubernetes test workload in a graduation project."
}
JSON
aws bedrock put-use-case-for-model-access --region "$bedrock_region" \
  --form-data "fileb://$bedrock_form_file"
rm -f "$bedrock_form_file"
```

같은 서울 리전에서 분당 요청 수와 토큰 수의 계정 적용 한도를 확인한다.
결과가 비어 있으면 그 한도의 계정 적용 값이 반환되지 않은 경우일 수 있다.
두 번째 명령은 AWS 기본값을 보여준다.
[Service Quotas 조회](https://docs.aws.amazon.com/cli/latest/reference/service-quotas/list-service-quotas.html)

```sh
bedrock_quota_query="Quotas[?contains(QuotaName, 'Claude Haiku 4.5') && contains(QuotaName, 'Global cross-region model inference')].[QuotaName,Value,QuotaCode]"
aws service-quotas list-service-quotas --region "$bedrock_region" \
  --service-code bedrock --query "$bedrock_quota_query" --output table
aws service-quotas list-aws-default-service-quotas --region "$bedrock_region" \
  --service-code bedrock --query "$bedrock_quota_query" --output table
```

## 3. AWS CLI 요청 한 번으로 확인

모델 활성화와 약관을 확인한 계정 소유자가 실행한다. 짧은 호출에도 토큰
사용료가 발생할 수 있다. 응답 텍스트와 사용량이 나오면 CLI 역할에서
모델을 호출할 수 있다.
[Bedrock Converse 명령](https://docs.aws.amazon.com/cli/latest/reference/bedrock-runtime/converse.html)

```sh
aws bedrock-runtime converse --region "$bedrock_region" \
  --model-id "$inference_profile_id" \
  --messages '[{"role":"user","content":[{"text":"Reply with OK."}]}]' \
  --inference-config '{"maxTokens":16,"temperature":0}' \
  --query '{Reply:output.message.content[0].text,Usage:usage}' --output json
```

- `AccessDeniedException`은 모델 이용 동의, 최초 이용 목적, 실행 역할의
  `bedrock:InvokeModel`, 글로벌 프로필 권한을 점검한다. 모델 조회에서
  `agreementAvailability.status`가 `PENDING`이면 활성화 완료를 기다린다.
- `ThrottlingException`은 분당 요청·토큰 한도와 일시적인 처리량 제한을
  확인한다. 한 번 성공한 경우에도 처음 활성화 중이면 계약 상태를 다시 확인한다.

## 4. 배포한 HolmesGPT에서도 확인

HolmesGPT 앱을 동기화한 뒤 운영자 kubeconfig가 가리키는 클러스터를 확인한다.
첫 터미널에서 아래 포트 전달을 연다.

```sh
kubectl config current-context
kubectl -n holmesgpt get deploy \
  -o custom-columns='NAME:.metadata.name,SERVICEACCOUNT:.spec.template.spec.serviceAccountName'
cluster_name=$(terraform -chdir=deploy/terraform/envs/eks-lab output -raw cluster_name)
aws eks list-pod-identity-associations --region "$bedrock_region" \
  --cluster-name "$cluster_name" --namespace holmesgpt --service-account holmes-bedrock
kubectl -n holmesgpt port-forward svc/holmesgpt-holmes 18081:80
```

배포와 AWS 연결 모두 `holmes-bedrock` 서비스 계정을 가리키는지 확인한다.
두 번째 터미널에서 HolmesGPT API가 모델 응답을 반환하는지 확인한다.

```sh
curl -fsS --max-time 120 http://127.0.0.1:18081/api/chat \
  -H 'Content-Type: application/json' \
  --data '{"ask":"Reply with exactly BEDROCK_OK without calling tools.","model":"bedrock-haiku"}' |
  jq -er '.analysis | select(type == "string" and contains("BEDROCK_OK"))'
```

응답에 `BEDROCK_OK`가 포함되면 HolmesGPT Pod에서 모델을 호출할 수 있다. CLI
호출만 성공하고 이 요청이 실패한다면 HolmesGPT 로그와 ServiceAccount의
Pod Identity 연결을 확인한다. 전시 전에 실제 시연용 Incident에서 HolmesGPT
분석 결과가 백엔드에 기록되는지도 확인한다. [Slack 전달 시험](k8s/eks-alert-smoke.md)은
알림 전달 경로를 확인하는 별도 시험이다.
