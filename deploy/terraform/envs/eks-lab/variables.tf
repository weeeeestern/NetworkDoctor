variable "name" {
  description = "Prefix for every resource (cluster, VPC, ALB, ...). Keep it short: ALB/TG names append '-backend' and must stay under 32 chars."
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
  type    = string
  default = "1.33"
}

variable "node_instance_types" {
  type    = list(string)
  default = ["t3.large"]
}

variable "node_min" {
  type    = number
  default = 1
}

variable "node_desired" {
  type    = number
  default = 2
}

variable "node_max" {
  type    = number
  default = 3
}

variable "domain" {
  description = "Public domain (registered at Gabia, DNS delegated to the Route53 zone this stack creates)."
  type        = string
  default     = "netdoc.cloud"
}
