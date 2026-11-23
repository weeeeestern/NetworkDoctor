# holmesgpt (NetworkDoctor RCA agent)

Wrapper around the official `holmes` chart 0.38.1 (vendored in `charts/`).

```text
Holmes pod (ns holmesgpt)
  init: fetch-skills  git fetch SKILLS_REF, sparse skills/  -> emptyDir -> /etc/holmes/networkdoctor-skills
  env:  LLM_GATEWAY_KEY (Secret holmes-llm), MODEL=gateway-sonnet (EKS: gateway-luna)
  LLM:  on-prem http://192.168.0.21:4000/v1   (LiteLLM gateway VM, deploy/llm-gateway/)
        EKS     http://llm-gateway.llm-gateway.svc:4000/v1   (deploy/helm/llm-gateway → Bedrock)
  metrics: prometheus-stack-kube-prom-prometheus.monitoring.svc:9090
  toolsets: kubernetes/core, kubernetes/logs, prometheus/metrics, skills, bash(extended)
```

## Before the first sync

On the EKS lab Terraform creates the Secret (deploy/terraform/envs/eks-lab/llm.tf);
skip this section there. On-prem the gateway virtual key is not in Git. Create the Secret once:

```bash
kubectl create namespace holmesgpt
kubectl -n holmesgpt create secret generic holmes-llm --from-literal=LLM_GATEWAY_KEY=<virtual key>
```

Without it the pod stays in `CreateContainerConfigError`.

## Models

| Holmes model name | Gateway alias | On-prem provider model | EKS (Bedrock) |
| --- | --- | --- | --- |
| `gateway-sonnet` (on-prem default) | `claude-sonnet` | Anthropic `claude-sonnet-5-5` | — |
| `gateway-haiku` | `claude-haiku` | Anthropic `claude-haiku-4-5-20251001` | `global.anthropic.claude-haiku-4-5-20251001-v1:0` |
| `gateway-luna` (EKS default) | `gpt-luna` | OpenAI `gpt-5.6-luna` | `global.openai.gpt-6-luna` (falls back to Haiku) |
| `gateway-luna-eval` | `gpt-luna-eval` | OpenAI `gpt-5.6-luna` | same as `gateway-luna`, no fallback |

The model list is per environment (`values-onprem-lab.yaml`, `values-eks-lab.yaml`).
Pick one per request with `"model": "gateway-sonnet"` in the `/api/chat` body,
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

Skills are fetched at pod start from `SKILLS_REF` (branch, tag or full SHA).
A merge that touches `skills/` runs the `skills-pin` workflow, which rewrites
the `# ci: skills-ref` line in `values.yaml` to that commit (`[skip ci]`).
The pod template changes, so Argo CD shows `holmesgpt` OutOfSync; sync it to
roll Holmes onto the new skills.
