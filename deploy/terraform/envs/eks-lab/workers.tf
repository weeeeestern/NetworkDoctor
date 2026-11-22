locals {
  worker_addons = toset(["coredns", "kube-proxy", "eks-pod-identity-agent"])
}

data "aws_eks_addon_version" "lab" {
  for_each = setunion(local.worker_addons, ["vpc-cni", "aws-ebs-csi-driver"])

  addon_name         = each.key
  kubernetes_version = var.cluster_version
  most_recent        = true
}

# Install the CNI with its own role before workers need Pod networking. IRSA
# lets the CNI start independently of the Pod Identity Agent. Keep default SNAT
# so packets can leave through the NAT gateway.
resource "aws_eks_addon" "vpc_cni" {
  cluster_name             = module.eks.cluster_name
  addon_name               = "vpc-cni"
  addon_version            = data.aws_eks_addon_version.lab["vpc-cni"].version
  service_account_role_arn = aws_iam_role.vpc_cni.arn

  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  depends_on = [aws_iam_role_policy_attachment.vpc_cni]
}

module "workers" {
  source  = "terraform-aws-modules/eks/aws//modules/eks-managed-node-group"
  version = "20.37.2"

  name                   = "${var.name}-workers"
  cluster_name           = module.eks.cluster_name
  cluster_version        = module.eks.cluster_version
  cluster_service_cidr   = module.eks.cluster_service_cidr
  vpc_security_group_ids = [module.eks.node_security_group_id]
  subnet_ids             = module.vpc.private_subnets
  instance_types         = var.node_instance_types
  ami_type               = "AL2023_x86_64_STANDARD"
  capacity_type          = "ON_DEMAND"
  enable_monitoring      = false # The demo uses Prometheus for monitoring.

  iam_role_attach_cni_policy = false
  metadata_options = {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  # Equal min, desired and max keep incident demo capacity fixed.
  min_size     = var.node_count
  desired_size = var.node_count
  max_size     = var.node_count

  # Workers need the private default routes for internet access during startup.
  depends_on = [aws_eks_addon.vpc_cni, module.vpc]
}

resource "aws_eks_addon" "workers" {
  for_each = local.worker_addons

  cluster_name  = module.eks.cluster_name
  addon_name    = each.key
  addon_version = data.aws_eks_addon_version.lab[each.key].version

  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  depends_on = [module.workers]
}

resource "aws_eks_addon" "ebs_csi" {
  cluster_name  = module.eks.cluster_name
  addon_name    = "aws-ebs-csi-driver"
  addon_version = data.aws_eks_addon_version.lab["aws-ebs-csi-driver"].version

  pod_identity_association {
    role_arn        = aws_iam_role.ebs_csi.arn
    service_account = "ebs-csi-controller-sa"
  }

  depends_on = [
    aws_eks_addon.workers["eks-pod-identity-agent"],
    aws_iam_role_policy_attachment.ebs_csi,
  ]
}
