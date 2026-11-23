output "cluster_name" {
  value = module.eks.cluster_name
}

output "configure_kubectl" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

output "lab_urls" {
  description = "Hosts served by deploy/helm/gateway-routes once the Argo apps are synced."
  value = {
    networkdoctor = "https://${var.domain}"
    grafana       = "https://grafana.${var.domain}"
  }
}

output "gateway_address_hint" {
  description = "The ALB comes from the Gateway, so its DNS name is in the Gateway status, not in Terraform state."
  value       = "kubectl -n ${local.gateway_namespace} get gateway ${local.gateway_name} -o jsonpath='{.status.addresses[0].value}'"
}

output "grafana_admin_password" {
  description = "Login for https://grafana.<domain> (user: admin)."
  value       = random_password.grafana_admin.result
  sensitive   = true
}
