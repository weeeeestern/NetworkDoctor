output "cluster_name" {
  value = module.eks.cluster_name
}

output "configure_kubectl" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

output "route53_name_servers" {
  description = "Set these 4 as 타사 네임서버 in My가비아 → 도메인 관리 → 네임서버 설정."
  value       = aws_route53_zone.this.name_servers
}

output "acm_certificate_arn" {
  description = "Paste into k8s/gateway.yaml LoadBalancerConfiguration (defaultCertificate)."
  value       = aws_acm_certificate.this.arn
}

output "app_url" {
  value = "https://${var.domain}"
}

output "gateway_address_hint" {
  description = "The public ALB is created from k8s Gateway resources, so its DNS lives in the Gateway status, not in Terraform state."
  value       = "kubectl -n networkdoctor get gateway networkdoctor -o jsonpath='{.status.addresses[0].value}'"
}
