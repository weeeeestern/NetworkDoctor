output "name_servers" {
  description = "Set these 4 as 타사 네임서버 in My가비아 → 도메인 관리 → 네임서버 설정."
  value       = aws_route53_zone.this.name_servers
}

output "zone_id" {
  value = aws_route53_zone.this.zone_id
}

output "certificate_arn" {
  description = "Wildcard certificate the EKS lab Gateway looks up by domain."
  value       = aws_acm_certificate.this.arn
}
