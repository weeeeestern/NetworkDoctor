# ---------------------------------------------------------------------------
# Grafana admin login. Grafana is public at grafana.<domain> with only its own
# login in front, so the password comes from here instead of the chart: Argo
# CD renders charts with `helm template`, where the chart's generated
# password would change on every sync. Read it with
#   terraform output -raw grafana_admin_password
# ---------------------------------------------------------------------------

resource "random_password" "grafana_admin" {
  length  = 32
  special = false
}

# Argo's monitoring app (CreateNamespace=true) reuses this namespace.
resource "kubectl_manifest" "monitoring_namespace" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Namespace"
    metadata   = { name = "monitoring" }
  })

  depends_on = [aws_eks_addon.workers]
}

# Referenced by grafana.admin.existingSecret in deploy/bootstrap/eks-lab.yaml.
resource "kubectl_manifest" "grafana_admin" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Secret"
    type       = "Opaque"
    metadata = {
      name      = "grafana-admin"
      namespace = "monitoring"
    }
    data = {
      admin-user     = base64encode("admin")
      admin-password = base64encode(random_password.grafana_admin.result)
    }
  })
  sensitive_fields = ["data"]

  depends_on = [kubectl_manifest.monitoring_namespace]
}
