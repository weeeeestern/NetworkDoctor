# EKS 알림 전달 리허설

운영자는 저장소 루트에서 실행한다. monitoring과 NetworkDoctor가 동기화되고
두 앱의 PVC가 `Bound`이며 [EKS 시연 런북](../README.md)의 Slack 설정이
적용된 상태에서 진행한다. 로컬에 `curl`과 `jq`가 필요하다.

임시 규칙 `NetworkDoctorEKSLabAlertSmoke`는 15초마다 상수식을 평가하고,
실행 시작 4분 뒤 첫 평가에서 해제된다. `smoke-eks-alert-delivery`의 합격은 Prometheus 발화,
새 백엔드 Incident, 팀 Slack 수신과 복구로 판정한다. 수집기 상태와 Holmes
분석 결과는 별도 항목으로 기록한다.

## 규칙 생성과 발화 확인

대상 EKS 컨텍스트를 확인한 뒤 두 터미널에서 각각 포트 전달을 유지한다.

```sh
kubectl -n monitoring port-forward svc/monitoring-kube-prometheus-prometheus 19090:9090
```

```sh
kubectl -n networkdoctor port-forward svc/networkdoctor-backend 18080:8080
```

세 번째 터미널에서 두 포트가 준비된 것을 확인하고 규칙을 생성한다.
`create`가 `AlreadyExists`를 반환하면 기존 리허설을 먼저 마친다.

```sh
curl -fsS http://127.0.0.1:19090/-/ready
curl -fsS http://127.0.0.1:18080/healthz
```

```sh
smoke_run=$(date -u +%Y%m%dT%H%M%SZ)
smoke_deadline=$(($(date -u +%s) + 240))
sed -e "s/__SMOKE_RUN__/$smoke_run/g" -e "s/< 0/< $smoke_deadline/" \
  deploy/terraform/k8s/eks-alert-smoke.example.yaml | kubectl create -f - &&
  printf 'Smoke run: %s\n' "$smoke_run"
```

같은 터미널에서 두 명령을 실행한다. 발화 결과나 Incident ID가 비어 있으면
15초 뒤 다시 확인한다. 첫 전달은 Alertmanager의 30초 대기 이후 시작한다.
팀 Slack 채널에서 `[FIRING] NetworkDoctor` 제목과 같은 실행 ID가 들어간
`[TEST] EKS alert delivery` 본문을 확인하고 메시지 링크를 기록한다.

```sh
curl -fsS http://127.0.0.1:19090/api/v1/alerts |
  jq -ce --arg run "$smoke_run" '
    select(.status == "success") |
    .data.alerts[] |
    select(.labels.alertname == "NetworkDoctorEKSLabAlertSmoke" and
           .labels.smoke_run == $run and .state == "firing") |
    {state, activeAt, labels}'
```

```sh
incident_id=$(curl -fsS http://127.0.0.1:18080/incidents |
  jq -er --arg run "$smoke_run" '
    .incidents[] |
    select(.rule_id == "smoke-eks-alert-delivery" and
           .symptom_summary == ("[TEST] EKS alert delivery " + $run) and
           .alert_status == "firing") |
    .incident_id') &&
  printf 'Firing Incident: %s\n' "$incident_id"
```

## 복구 확인과 정리

규칙을 유지한 채 발화 조건이 만료되도록 기다린다. 실행 시작 7분 이내에 아래
첫 명령이 `true`를 반환하고, 두 번째 명령이 같은 Incident의
`alert_status: resolved`와 `recovery_status: resolved`를 반환해야 합격이다.
팀 Slack 채널에서 같은 실행 ID의 `[RESOLVED] NetworkDoctor` 메시지도 확인한다.

```sh
curl -fsS http://127.0.0.1:19090/api/v1/alerts |
  jq -e --arg run "$smoke_run" '
    .status == "success" and all(.data.alerts[]; .labels.smoke_run != $run)'

curl -fsS "http://127.0.0.1:18080/incidents/$incident_id" |
  jq -ce --arg run "$smoke_run" --arg id "$incident_id" '
    select(.incident_id == $id and .alert_labels.smoke_run == $run and
           .alert_status == "resolved" and .recovery_status == "resolved") |
    {incident_id, cluster, alert_status, recovery_status, starts_at, ends_at}'
```

실행 ID, Incident ID, 두 Slack 메시지 링크를 리허설 기록에 남긴다. 보고서를
보관하려면 Incident 상세 화면에서 내려받거나 다음 명령을 실행한다.

```sh
curl -fsS "http://127.0.0.1:18080/incidents/$incident_id/report.md" \
  -o "eks-alert-smoke-$smoke_run.md"
```

누락된 단계가 있으면 실패로 기록한다. 복구 확인 또는 확인 제한 시간 이후
임시 규칙을 삭제하고 두 포트 전달을 종료한다.

```sh
kubectl -n networkdoctor delete prometheusrule networkdoctor-eks-alert-smoke --ignore-not-found
```
