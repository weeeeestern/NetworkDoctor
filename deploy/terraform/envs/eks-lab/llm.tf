# ---------------------------------------------------------------------------
# Shared key between HolmesGPT and the in-cluster LLM gateway (LiteLLM,
# deploy/helm/llm-gateway). The gateway has no database, so its master key is
# the only key; Holmes sends it as the OpenAI API key. Both Secrets come from
# here so neither app needs a hand-made Secret before its first sync.
# ---------------------------------------------------------------------------

resource "random_password" "llm_gateway_master_key" {
  length  = 48
  special = false
}

locals {
  llm_gateway_key = "sk-${random_password.llm_gateway_master_key.result}"
}

# Argo's llm-gateway and holmesgpt apps (CreateNamespace=true) reuse these.
resource "kubectl_manifest" "llm_namespaces" {
  for_each = toset([local.llm_gateway_namespace, "holmesgpt"])

  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Namespace"
    metadata   = { name = each.key }
  })

  depends_on = [aws_eks_addon.workers]
}

# The same key under the name each side reads:
#   gateway  LITELLM_MASTER_KEY (deploy/helm/llm-gateway)
#   Holmes   LLM_GATEWAY_KEY, as an env var via holmes.extraEnvVarsSecrets
# Pods read it only at start. If the key is ever replaced (taint or state
# loss), restart both deployments, or Holmes gets 401 from the gateway.
locals {
  llm_key_secrets = {
    gateway = { namespace = local.llm_gateway_namespace, name = "llm-gateway", key = "LITELLM_MASTER_KEY" }
    holmes  = { namespace = "holmesgpt", name = "holmes-llm", key = "LLM_GATEWAY_KEY" }
  }
}

resource "kubectl_manifest" "llm_key" {
  for_each = local.llm_key_secrets

  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    type       = "Opaque"
    metadata = {
      name      = each.value.name
      namespace = each.value.namespace
    }
    data = {
      (each.value.key) = base64encode(local.llm_gateway_key)
    }
  })
  sensitive_fields = ["data"]

  depends_on = [kubectl_manifest.llm_namespaces]
}
