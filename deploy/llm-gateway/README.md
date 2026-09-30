# LLM Gateway (LiteLLM on a VM outside the cluster)

The cluster never sees provider keys. HolmesGPT (and later the backend) get a
LiteLLM **virtual key** and an OpenAI-compatible base URL; the real Anthropic
/ OpenAI / Bedrock credentials live only in `.env` on this VM.

```text
HolmesGPT (in cluster) --OpenAI API + virtual key--> http://192.168.0.21:4000/v1
                                                        LiteLLM Proxy (VM 111)
                                                          └─ provider keys (.env) --> Anthropic / OpenAI / Bedrock
```

Lab VM: Proxmox VM 111 `gateway-litellm`, Ubuntu 24.04, 2 vCPU / 4 GB / 32 GB,
static `192.168.0.21`.

## Install (once, on the VM)

```bash
# Docker
sudo apt-get update && sudo apt-get install -y ca-certificates curl qemu-guest-agent
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update && sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
sudo usermod -aG docker "$USER"   # re-login afterwards

# Gateway files
mkdir -p ~/llm-gateway && cd ~/llm-gateway
# copy docker-compose.yml, config.yaml, .env.example from this directory
cp .env.example .env
$EDITOR .env            # LITELLM_MASTER_KEY, POSTGRES_PASSWORD (twice), provider key
docker compose up -d
docker compose logs -f litellm   # wait for "Uvicorn running on 0.0.0.0:4000"
```

## Verify

```bash
export MK="$(grep ^LITELLM_MASTER_KEY .env | cut -d= -f2)"
curl -s http://localhost:4000/health/liveliness
curl -s -H "Authorization: Bearer $MK" http://localhost:4000/v1/models | jq '.data[].id'
curl -s -H "Authorization: Bearer $MK" -H 'Content-Type: application/json' \
  http://localhost:4000/v1/chat/completions \
  -d '{"model":"claude-haiku","messages":[{"role":"user","content":"Say OK."}],"max_tokens":5}'
```

From the control plane the same calls must work against `http://192.168.0.21:4000`.

## Issue the key the cluster will use

One virtual key per consumer, with a budget, so a leaked cluster key can be
revoked without touching provider credentials.

```bash
curl -s -X POST http://localhost:4000/key/generate \
  -H "Authorization: Bearer $MK" -H 'Content-Type: application/json' \
  -d '{"key_alias":"holmesgpt-onprem-lab","models":["claude-sonnet","claude-haiku","gpt-luna","gpt-6-luna"],"max_budget":20,"budget_duration":"30d"}' \
  | jq -r .key
```

Store the printed `sk-...` in a Kubernetes Secret (`holmes-llm` in the Holmes
namespace). Holmes then needs only:

```yaml
baseURL: http://192.168.0.21:4000/v1
model:   openai/claude-sonnet      # LiteLLM speaks the OpenAI protocol
secret:  holmes-llm / OPENAI_API_KEY
```

Swapping LiteLLM for another gateway later changes those three values and
nothing else. Comparing models is a one-line change of `model`
(`openai/claude-sonnet` vs `openai/gpt-luna`); both sit behind the same key.

## Operations

```bash
docker compose ps
docker compose logs --since 10m litellm
docker compose pull && docker compose up -d      # upgrade (pin the tag in docker-compose.yml first)
curl -s -H "Authorization: Bearer $MK" http://localhost:4000/spend/logs | jq length   # usage
```

The proxy listens on all interfaces of the VM but the VM only has a LAN
address; do not port-forward 4000 on the router.
