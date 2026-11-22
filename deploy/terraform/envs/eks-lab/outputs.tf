output "cluster_name" {
  value = module.eks.cluster_name
}

output "configure_kubectl" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

output "team_alb_dns_name" {
  description = "DNS target for the team's existing HTTPS subdomain."
  value       = aws_lb.team.dns_name
}

output "team_alb_target_group_arn" {
  description = "Backend target group ARN for AWS CLI health checks."
  value       = aws_lb_target_group.team_backend.arn
}
