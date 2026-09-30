# holmesgpt (NetworkDoctor RCA agent)

Wrapper around the official `holmes` chart 0.38.1 (vendored in `charts/`).

```text
Holmes pod (ns holmesgpt)
  init: fetch-skills  git clone --sparse skills/  -> emptyDir -> /etc/holmes/networkdoctor-skills
  env:  LLM_GATEWAY_KEY (Secret holmes-llm), MODEL=gateway-sonnet
  LLM:  http://192.168.0.21:4000/v1   (LiteLLM gateway, deploy/llm-gateway/)
  metrics: prometheus-stack-kube-prom-prometheus.monitoring.svc:9090
  toolsets: kubernetes/core, kubernetes/logs, prometheus/metrics, skills, bash(extended)
```

## Before the first sync

The gateway virtual key is not in Git. Create the Secret once:

```bash
kubectl create namespace holmesgpt
kubectl -n holmesgpt create secret generic holmes-llm --from-literal=LLM_GATEWAY_KEY=<virtual key>
```

Without it the pod stays in `CreateContainerConfigError`.

## Models

| Holmes model name | Gateway alias | Provider model |
| --- | --- | --- |
| `gateway-sonnet` (default) | `claude-sonnet` | Anthropic `claude-sonnet-5-5` |
| `gateway-haiku` | `claude-haiku` | Anthropic `claude-haiku-4-5-20251001` |
| `gateway-luna` | `gpt-luna` | OpenAI `gpt-5.6-luna` |

Pick one per request with `"model": "gateway-luna"` in the `/api/chat` body,
or change the `MODEL` env var for the default.

## Try it

```bash
kubectl -n holmesgpt port-forward svc/holmesgpt-holmes 8080:80 &
curl -s localhost:8080/api/chat -H 'Content-Type: application/json' -d '{
  "ask": "NetworkDoctorTCPRetransmitBurstHigh fired on node worker01 at 2026-09-28T08:09:20Z (rule_id smoke-1). Investigate using the network-congestion skill and answer in its output schema.",
  "model": "gateway-sonnet"
}' | jq -r .analysis
```

## Skill updates

Skills are fetched at pod start. After merging a skill change to `main`:

```bash
kubectl -n holmesgpt rollout restart deploy/holmesgpt-holmes
```
