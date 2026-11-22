variable "domain" {
  description = "Public zone for the lab. Use a subdomain (e.g. lab.netdoc.cloud) to delegate only that name instead of the whole domain."
  type        = string
  default     = "netdoc.cloud"
}

variable "region" {
  description = "Region of the certificate. Must match the EKS lab region, since an ALB only uses certificates from its own region."
  type        = string
  default     = "ap-northeast-2"
}
