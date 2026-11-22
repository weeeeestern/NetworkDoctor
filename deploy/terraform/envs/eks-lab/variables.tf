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

variable "availability_zones" {
  description = "Two AZs for the public (ALB, NAT) and private (EKS) subnets. The first one's node group is labelled networkdoctor.io/stateful=true and hosts Prometheus and the backend (EBS)."
  type        = list(string)
  default     = ["ap-northeast-2a", "ap-northeast-2c"]

  validation {
    condition     = length(var.availability_zones) == 2
    error_message = "Use exactly two availability zones."
  }
}

variable "node_instance_types" {
  type    = list(string)
  default = ["t3a.medium"]
}

variable "node_counts" {
  description = "Fixed worker count per AZ, in availability_zones order. One node group per AZ keeps replacements in the same AZ as the EBS volumes."
  type        = list(number)
  default     = [2, 1]

  validation {
    condition     = length(var.node_counts) == 2 && alltrue([for n in var.node_counts : n >= 1])
    error_message = "Give one count of at least 1 for each of the two availability zones."
  }
}

variable "domain" {
  description = "Route53 zone and wildcard ACM certificate created by envs/dns. Lab hosts are <name>.<domain>."
  type        = string
  default     = "netdoc.cloud"
}

variable "admin_role_arns" {
  description = "IAM role ARNs that get cluster-admin access entries, besides the GitHub Actions role from envs/bootstrap. Include the role you run Terraform or kubectl with locally, or that role is locked out of the cluster."
  type        = set(string)
  default     = []
}

variable "team_role_arns" {
  description = "IAM role ARNs for trusted lab operators who may forward to pods in the NetworkDoctor namespace. Use separate roles from the cluster creator role."
  type        = set(string)
  default     = []
}
