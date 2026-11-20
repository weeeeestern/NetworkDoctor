# ---------------------------------------------------------------------------
# VPC — private subnets for the EKS control plane interfaces, public subnets
# for workers. Pods reach the internet through the worker's public IPv4.
# ---------------------------------------------------------------------------

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 3)
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.21.0"

  name = var.name
  cidr = var.vpc_cidr

  azs = local.azs
  # 10.80.0.0/20, 10.80.16.0/20, 10.80.32.0/20
  private_subnets = [for i in range(length(local.azs)) : cidrsubnet(var.vpc_cidr, 4, i)]
  # 10.80.48.0/24, 10.80.49.0/24, 10.80.50.0/24
  public_subnets = [for i in range(length(local.azs)) : cidrsubnet(var.vpc_cidr, 8, i + 48)]

  create_igw              = true
  map_public_ip_on_launch = true
  enable_nat_gateway      = false
  enable_dns_hostnames    = true
}

# ---------------------------------------------------------------------------
# EKS control plane. Worker and add-on ordering is defined separately.
# ---------------------------------------------------------------------------

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "20.37.2"

  cluster_name    = var.name
  cluster_version = var.cluster_version

  vpc_id      = module.vpc.vpc_id
  subnet_ids  = module.vpc.private_subnets
  enable_irsa = true

  cluster_upgrade_policy = {
    support_type = "STANDARD"
  }

  # Workers use the private endpoint. Team kubectl and Terraform's Helm
  # provider use the public endpoint from their allowlisted IPs.
  cluster_endpoint_private_access          = true
  cluster_endpoint_public_access           = true
  cluster_endpoint_public_access_cidrs     = var.team_api_cidrs
  enable_cluster_creator_admin_permissions = true

  access_entries = {
    for role_arn in var.team_role_arns : role_arn => {
      principal_arn     = role_arn
      kubernetes_groups = ["networkdoctor-lab-operators"]
    }
  }
}

# ---------------------------------------------------------------------------
# Dedicated IAM roles for the managed add-ons.
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "vpc_cni_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [module.eks.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${module.eks.oidc_provider}:sub"
      values   = ["system:serviceaccount:kube-system:aws-node"]
    }

    condition {
      test     = "StringEquals"
      variable = "${module.eks.oidc_provider}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "vpc_cni" {
  name               = "${var.name}-vpc-cni"
  assume_role_policy = data.aws_iam_policy_document.vpc_cni_assume.json
}

resource "aws_iam_role_policy_attachment" "vpc_cni" {
  role       = aws_iam_role.vpc_cni.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
}

data "aws_iam_policy_document" "pod_identity_assume" {
  statement {
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }

    actions = ["sts:AssumeRole", "sts:TagSession"]
  }
}

# EBS CSI driver
resource "aws_iam_role" "ebs_csi" {
  name               = "${var.name}-ebs-csi"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEBSCSIDriverPolicyV2"
}
