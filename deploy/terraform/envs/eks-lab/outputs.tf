output "cluster_name" {
  value = module.eks.cluster_name
}

output "configure_kubectl" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

output "lab_urls" {
  description = "Hosts served by deploy/helm/gateway-routes once the Argo apps are synced."
  value = {
    networkdoctor = "https://nd.${var.domain}"
    grafana       = "https://grafana.${var.domain}"
    holmes        = "https://holmes.${var.domain}"
  }
}

output "gateway_address_hint" {
  description = "The ALB comes from the Gateway, so its DNS name is in the Gateway status, not in Terraform state."
  value       = "kubectl -n ${local.gateway_namespace} get gateway ${local.gateway_name} -o jsonpath='{.status.addresses[0].value}'"
}

output "stateful_zone" {
  description = "Zone the Prometheus and backend nodeSelectors must name."
  value       = var.availability_zones[0]
}
