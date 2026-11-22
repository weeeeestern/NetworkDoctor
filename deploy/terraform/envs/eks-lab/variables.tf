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
  description = "Two AZs for the public (ALB, NAT) and private (EKS) subnets. The first one hosts the stateful pods (Prometheus, backend); keep it in sync with the zone nodeSelectors in deploy/bootstrap/eks-lab.yaml and deploy/helm/networkdoctor/values-eks-lab.yaml."
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

variable "team_api_cidrs" {
  description = "Individual team and Terraform public IPv4 addresses in /32 form, for the EKS API and the Gateway ALB."
  type        = list(string)

  validation {
    condition = length(var.team_api_cidrs) > 0 && alltrue([
      for cidr in var.team_api_cidrs : try(cidrnetmask(cidr) == "255.255.255.255", false)
    ])
    error_message = "Enter individual team public IPv4 addresses as /32 CIDRs."
  }
}

variable "team_role_arns" {
  description = "IAM role ARNs for trusted lab operators who may forward to pods in the NetworkDoctor namespace. Use separate roles from the cluster creator role."
  type        = set(string)
  default     = []
}
