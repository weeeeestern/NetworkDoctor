variable "name" {
  description = "Name of the EKS lab cluster and prefix for its resources."
  type        = string
  default     = "nd-eks-lab"
}

variable "region" {
  type    = string
  default = "ap-northeast-2"
}

variable "vpc_cidr" {
  type    = string
  default = "10.80.0.0/16"
}

variable "cluster_version" {
  description = "EKS standard-support Kubernetes version compatible with the pinned Cilium chart."
  type        = string
  default     = "1.35"
}

variable "node_instance_types" {
  type    = list(string)
  default = ["t3a.medium"]
}

variable "node_count" {
  description = "Fixed number of managed workers for the incident demo."
  type        = number
  default     = 3
}

variable "team_api_cidrs" {
  description = "Individual team and Terraform public IPv4 addresses in /32 form, for the API and team HTTPS listener."
  type        = list(string)

  validation {
    condition = length(var.team_api_cidrs) > 0 && alltrue([
      for cidr in var.team_api_cidrs : try(cidrnetmask(cidr) == "255.255.255.255", false)
    ])
    error_message = "Enter individual team public IPv4 addresses as /32 CIDRs."
  }
}

variable "team_alb_certificate_arn" {
  description = "Issued ACM certificate for the team's subdomain in the same AWS region as the ALB."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^arn:aws:acm:[a-z0-9-]+:[0-9]{12}:certificate/[0-9a-f-]+$", var.team_alb_certificate_arn))
    error_message = "Enter the ARN of a valid ACM certificate for the team subdomain."
  }
}

variable "team_role_arns" {
  description = "IAM role ARNs for trusted lab operators who may forward to pods in the NetworkDoctor namespace. Use separate roles from the cluster creator role."
  type        = set(string)
  default     = []
}
