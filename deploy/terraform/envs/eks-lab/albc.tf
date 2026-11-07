# ---------------------------------------------------------------------------
# AWS Load Balancer Controller (v2.13+, Gateway API enabled)
#
# External access uses the Kubernetes Gateway API: the controller watches
# Gateway/HTTPRoute resources (deploy/terraform/k8s/gateway.yaml) and
# provisions an internet-facing ALB from them. Requires the Gateway API CRDs
# to be installed first — see deploy/terraform/README.md.
# ---------------------------------------------------------------------------

resource "aws_iam_policy" "albc" {
  name = "${var.name}-aws-load-balancer-controller"
  # Official policy for controller v2.13 (docs/install/iam_policy.json).
  policy = file("${path.module}/iam-policy-aws-load-balancer-controller.json")
}

resource "aws_iam_role" "albc" {
  name               = "${var.name}-aws-load-balancer-controller"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy_attachment" "albc" {
  role       = aws_iam_role.albc.name
  policy_arn = aws_iam_policy.albc.arn
}

resource "aws_eks_pod_identity_association" "albc" {
  cluster_name    = module.eks.cluster_name
  namespace       = "kube-system"
  service_account = "aws-load-balancer-controller"
  role_arn        = aws_iam_role.albc.arn
}

resource "helm_release" "albc" {
  name       = "aws-load-balancer-controller"
  repository = "https://aws.github.io/eks-charts"
  chart      = "aws-load-balancer-controller"
  version    = "1.13.0" # controller v2.13.0, matches the IAM policy file
  namespace  = "kube-system"

  set {
    name  = "clusterName"
    value = module.eks.cluster_name
  }
  set {
    name  = "region"
    value = var.region
  }
  set {
    name  = "vpcId"
    value = module.vpc.vpc_id
  }
  set {
    name  = "serviceAccount.name"
    value = "aws-load-balancer-controller"
  }

  # Gateway API support ships behind feature gates in v2.13.
  set {
    name  = "controllerConfig.featureGates.ALBGatewayAPI"
    value = "true"
  }
  set {
    name  = "controllerConfig.featureGates.NLBGatewayAPI"
    value = "true"
  }

  # The Pod Identity association must exist before the controller pod starts,
  # otherwise it comes up without credentials and needs a restart.
  depends_on = [
    module.eks,
    aws_eks_pod_identity_association.albc,
    helm_release.cilium, # start already Cilium-managed (no restart needed)
  ]
}
