variable "region" {
  type    = string
  default = "ap-northeast-2"
}

variable "github_repository" {
  description = "owner/name of the repository whose GitHub Actions may assume the Terraform role."
  type        = string
  default     = "weeeeestern/NetworkDoctor"
}

variable "lab_name" {
  description = "The eks-lab `name` variable. The role may manage only IAM resources with this prefix."
  type        = string
  default     = "nd-eks-lab"
}

variable "create_github_oidc_provider" {
  description = "An account has at most one GitHub OIDC provider. Set false if it already exists; the existing one is then looked up."
  type        = bool
  default     = true
}
