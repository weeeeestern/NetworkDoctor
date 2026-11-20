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
  description = "Public IPv4 CIDRs for team members and the machine running Terraform (use /32 for a single IP)."
  type        = list(string)

  validation {
    condition = length(var.team_api_cidrs) > 0 && alltrue([
      for cidr in var.team_api_cidrs : try(cidrnetmask(cidr) != "0.0.0.0", false)
    ])
    error_message = "Enter at least one IPv4 CIDR for the team. A /0 range would expose the API to the entire internet."
  }
}

variable "team_role_arns" {
  description = "IAM role ARNs for trusted lab operators who may forward to pods in the NetworkDoctor namespace. Use separate roles from the cluster creator role."
  type        = set(string)
  default     = []
}
