# ---------------------------------------------------------------------------
# VPC — two AZs. Public subnets hold the Gateway ALB and a single zonal NAT
# gateway; private subnets hold the EKS interfaces, workers and the Bedrock
# endpoint. The lab accepts a shared egress dependency.
# ---------------------------------------------------------------------------

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.21.0"

  name = var.name
  cidr = var.vpc_cidr

  azs = var.availability_zones
  # 10.80.0.0/20, 10.80.16.0/20
  private_subnets = [for i in range(length(var.availability_zones)) : cidrsubnet(var.vpc_cidr, 4, i)]
  # 10.80.48.0/24, 10.80.49.0/24
  public_subnets = [for i in range(length(var.availability_zones)) : cidrsubnet(var.vpc_cidr, 8, i + 48)]

  create_igw              = true
  map_public_ip_on_launch = false
  enable_nat_gateway      = true
  single_nat_gateway      = true
  enable_dns_hostnames    = true

  # Subnet discovery for the AWS Load Balancer Controller.
  public_subnet_tags = {
    "kubernetes.io/role/elb" = 1
  }
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = 1
  }
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

  # Workers use the private endpoint. kubectl, GitHub Actions and Terraform's
  # Helm/kubectl providers use the public endpoint, open to any source IP
  # because the team and the Actions runners have no fixed addresses. Every
  # request still needs an IAM-signed token and a matching access entry.
  cluster_endpoint_private_access      = true
  cluster_endpoint_public_access       = true
  cluster_endpoint_public_access_cidrs = ["0.0.0.0/0"]

  # Admins are listed explicitly instead of "whoever created the cluster", so
  # the cluster stays reachable from both GitHub Actions and the operator.
  enable_cluster_creator_admin_permissions = false

  access_entries = merge(
    {
      for role_arn in var.team_role_arns : role_arn => {
        principal_arn     = role_arn
        kubernetes_groups = ["networkdoctor-lab-operators"]
      }
    },
    {
      for role_arn in local.admin_role_arns : role_arn => {
        principal_arn = role_arn
        policy_associations = {
          admin = {
            policy_arn   = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
            access_scope = { type = "cluster" }
          }
        }
      }
    },
    {
      for role_arn in data.aws_iam_roles.github_actions_plan.arns : role_arn => {
        principal_arn = role_arn
        policy_associations = {
          view = {
            policy_arn   = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminViewPolicy"
            access_scope = { type = "cluster" }
          }
        }
      }
    },
  )
}

# The GitHub Actions roles from envs/bootstrap, when they exist. A lookup by
# name pattern returns an empty set instead of failing before bootstrap.
# The apply role is a cluster admin; the plan role may only read (including
# Secrets, which plans of the Helm releases need).
data "aws_iam_roles" "github_actions" {
  name_regex = "^networkdoctor-github-actions$"
}

data "aws_iam_roles" "github_actions_plan" {
  name_regex = "^networkdoctor-github-actions-plan$"
}

locals {
  admin_role_arns = setunion(data.aws_iam_roles.github_actions.arns, var.admin_role_arns)
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
