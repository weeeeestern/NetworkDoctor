# ---------------------------------------------------------------------------
# One-time setup for running Terraform from GitHub Actions: the S3 bucket
# that holds the dns and eks-lab state, and the IAM role Actions assumes
# through GitHub's OIDC provider (no stored AWS keys).
#
# Apply this once from a local admin session. Its own state stays local
# (terraform.tfstate here, gitignored); keep that file, or move it into the
# new bucket afterwards (deploy/terraform/README.md).
# ---------------------------------------------------------------------------

data "aws_caller_identity" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
}

# ---------------------------------------------------------------------------
# State bucket. Versioned so a bad apply can be rolled back to an earlier
# state object; locking uses S3 lock files (use_lockfile), not DynamoDB.
# ---------------------------------------------------------------------------

resource "aws_s3_bucket" "state" {
  bucket = "networkdoctor-tfstate-${local.account_id}"

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

data "aws_iam_policy_document" "state_bucket" {
  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.state.arn,
      "${aws_s3_bucket.state.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id
  policy = data.aws_iam_policy_document.state_bucket.json

  depends_on = [aws_s3_bucket_public_access_block.state]
}

# ---------------------------------------------------------------------------
# GitHub OIDC → IAM role
# ---------------------------------------------------------------------------

resource "aws_iam_openid_connect_provider" "github" {
  count = var.create_github_oidc_provider ? 1 : 0

  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
}

data "aws_iam_openid_connect_provider" "github" {
  count = var.create_github_oidc_provider ? 0 : 1

  url = "https://token.actions.githubusercontent.com"
}

locals {
  github_oidc_provider_arn = (var.create_github_oidc_provider
    ? aws_iam_openid_connect_provider.github[0].arn
  : data.aws_iam_openid_connect_provider.github[0].arn)
}

# Any workflow run in this repository: pull requests from its own branches
# (plan) and manual dispatches (apply/destroy). GitHub issues no OIDC token
# to pull requests from forks, so outside contributors never get the role.
data "aws_iam_policy_document" "github_actions_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [local.github_oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.github_repository}:*"]
    }
  }
}

# The name is fixed: envs/eks-lab finds this role by name and makes it a
# cluster admin.
resource "aws_iam_role" "github_actions" {
  name                 = "networkdoctor-github-actions"
  assume_role_policy   = data.aws_iam_policy_document.github_actions_assume.json
  max_session_duration = 7200 # an EKS create or destroy can take ~30 minutes
}

# Everything except IAM (VPC, EKS, EC2, ELB, Route53, ACM, KMS, S3, ...).
resource "aws_iam_role_policy_attachment" "github_actions_power_user" {
  role       = aws_iam_role.github_actions.name
  policy_arn = "arn:aws:iam::aws:policy/PowerUserAccess"
}

# IAM only for the lab's own roles/policies (name prefix) and its cluster
# OIDC provider. Managed-policy attachments are limited to the EKS, ECR and
# EBS policies the lab uses, so the role cannot attach AdministratorAccess to
# a lab role. Inline role policies are still allowed on lab roles, so this
# narrows mistakes rather than forming a hard privilege boundary.
data "aws_iam_policy_document" "github_actions_iam" {
  statement {
    sid = "ReadIam"
    actions = [
      "iam:Get*",
      "iam:List*",
    ]
    resources = ["*"]
  }

  statement {
    sid = "ManageLabIam"
    actions = [
      "iam:CreateRole",
      "iam:DeleteRole",
      "iam:UpdateRole",
      "iam:UpdateAssumeRolePolicy",
      "iam:TagRole",
      "iam:UntagRole",
      "iam:PutRolePolicy",
      "iam:DeleteRolePolicy",
      "iam:DetachRolePolicy",
      "iam:PassRole",
      "iam:CreatePolicy",
      "iam:DeletePolicy",
      "iam:CreatePolicyVersion",
      "iam:DeletePolicyVersion",
      "iam:SetDefaultPolicyVersion",
      "iam:TagPolicy",
      "iam:UntagPolicy",
      "iam:CreateInstanceProfile",
      "iam:DeleteInstanceProfile",
      "iam:AddRoleToInstanceProfile",
      "iam:RemoveRoleFromInstanceProfile",
      "iam:TagInstanceProfile",
      "iam:UntagInstanceProfile",
      "iam:CreateOpenIDConnectProvider",
      "iam:DeleteOpenIDConnectProvider",
      "iam:UpdateOpenIDConnectProviderThumbprint",
      "iam:AddClientIDToOpenIDConnectProvider",
      "iam:RemoveClientIDFromOpenIDConnectProvider",
      "iam:TagOpenIDConnectProvider",
      "iam:UntagOpenIDConnectProvider",
    ]
    resources = [
      "arn:aws:iam::${local.account_id}:role/${var.lab_name}-*",
      "arn:aws:iam::${local.account_id}:policy/${var.lab_name}-*",
      "arn:aws:iam::${local.account_id}:instance-profile/${var.lab_name}-*",
      "arn:aws:iam::${local.account_id}:oidc-provider/oidc.eks.${var.region}.amazonaws.com/*",
    ]
  }

  statement {
    sid       = "AttachLabPolicies"
    actions   = ["iam:AttachRolePolicy"]
    resources = ["arn:aws:iam::${local.account_id}:role/${var.lab_name}-*"]

    condition {
      test     = "ArnLike"
      variable = "iam:PolicyARN"
      values = [
        "arn:aws:iam::aws:policy/AmazonEKS*",
        "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistry*",
        "arn:aws:iam::aws:policy/AmazonEBSCSIDriverPolicy*",
        "arn:aws:iam::${local.account_id}:policy/${var.lab_name}-*",
      ]
    }
  }
}

resource "aws_iam_role_policy" "github_actions_iam" {
  name   = "lab-iam"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.github_actions_iam.json
}
