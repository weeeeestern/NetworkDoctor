# ---------------------------------------------------------------------------
# Team HTTPS entry — Kubernetes Gateway API on the AWS Load Balancer
# Controller (LBC). The controller turns the Gateway below into one
# internet-facing ALB and each HTTPRoute (deploy/helm/gateway-routes, an Argo
# app) into listener rules with IP targets, so Services stay ClusterIP.
#
# Terraform owns the Gateway so `terraform destroy` deletes it first and
# waits for the controller to remove the ALB before the controller and VPC
# go away. A Gateway left behind would keep ALB ENIs in the public subnets.
# ---------------------------------------------------------------------------

locals {
  # Standard channel CRDs. LBC v3.6 documents Gateway API v1.6.
  gateway_api_version = "v1.6.3"
  gateway_namespace   = "gateway"
  gateway_name        = "lab"
  # Namespaces whose HTTPRoutes may attach to the lab Gateway.
  gateway_route_namespaces = ["networkdoctor", "monitoring", "holmesgpt"]
}

data "http" "gateway_api_crds" {
  url = "https://github.com/kubernetes-sigs/gateway-api/releases/download/${local.gateway_api_version}/standard-install.yaml"
}

data "kubectl_file_documents" "gateway_api_crds" {
  content = data.http.gateway_api_crds.response_body
}

# Server-side apply: the CRDs are too large for the last-applied annotation.
resource "kubectl_manifest" "gateway_api_crds" {
  for_each = data.kubectl_file_documents.gateway_api_crds.manifests

  yaml_body         = each.value
  server_side_apply = true
  wait              = true

  depends_on = [aws_eks_addon.workers]
}

# ---------------------------------------------------------------------------
# AWS Load Balancer Controller. Its chart ships the gateway.k8s.aws CRDs
# (LoadBalancerConfiguration, TargetGroupConfiguration, ...). The Gateway
# controllers start only when the Gateway API CRDs already exist.
# ---------------------------------------------------------------------------

resource "aws_iam_policy" "albc" {
  name = "${var.name}-aws-load-balancer-controller"
  # Official policy for controller v3.6.2 (docs/install/iam_policy.json).
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
  version    = "3.6.2" # controller v3.6.2, matches the IAM policy file
  namespace  = "kube-system"

  values = [yamlencode({
    clusterName  = module.eks.cluster_name
    region       = var.region
    vpcId        = module.vpc.vpc_id
    replicaCount = 1 # lab capacity; no HA needed
    serviceAccount = {
      name = "aws-load-balancer-controller"
    }
  })]

  # The Pod Identity association must exist before the controller pod starts,
  # otherwise it comes up without credentials and needs a restart.
  depends_on = [
    kubectl_manifest.gateway_api_crds,
    aws_eks_pod_identity_association.albc,
    aws_iam_role_policy_attachment.albc,
    helm_release.cilium, # start already Cilium-managed (no restart needed)
  ]
}

# ---------------------------------------------------------------------------
# The lab Gateway: one internet-facing HTTPS:443 listener with the wildcard
# certificate from envs/dns. There is no login in front of it, so each
# HTTPRoute limits what it exposes (deploy/helm/gateway-routes).
# ---------------------------------------------------------------------------

data "aws_acm_certificate" "lab" {
  domain      = var.domain
  statuses    = ["ISSUED"]
  most_recent = true
}

resource "kubectl_manifest" "gateway_namespace" {
  yaml_body = yamlencode({
    apiVersion = "v1"
    kind       = "Namespace"
    metadata   = { name = local.gateway_namespace }
  })

  depends_on = [aws_eks_addon.workers]
}

resource "kubectl_manifest" "gateway_class" {
  yaml_body = yamlencode({
    apiVersion = "gateway.networking.k8s.io/v1"
    kind       = "GatewayClass"
    metadata   = { name = "alb" }
    spec       = { controllerName = "gateway.k8s.aws/alb" }
  })

  depends_on = [helm_release.albc]
}

resource "kubectl_manifest" "gateway_lb_config" {
  yaml_body = yamlencode({
    apiVersion = "gateway.k8s.aws/v1"
    kind       = "LoadBalancerConfiguration"
    metadata = {
      name      = local.gateway_name
      namespace = local.gateway_namespace
    }
    spec = {
      scheme = "internet-facing"
      listenerConfigurations = [{
        protocolPort       = "HTTPS:443"
        defaultCertificate = data.aws_acm_certificate.lab.arn
      }]
    }
  })

  depends_on = [helm_release.albc, kubectl_manifest.gateway_namespace]
}

# wait = true: on destroy, block until the controller has deleted the ALB and
# released its finalizer, so the controller is still running while it does.
resource "kubectl_manifest" "gateway" {
  yaml_body = yamlencode({
    apiVersion = "gateway.networking.k8s.io/v1"
    kind       = "Gateway"
    metadata = {
      name      = local.gateway_name
      namespace = local.gateway_namespace
    }
    spec = {
      gatewayClassName = "alb"
      infrastructure = {
        parametersRef = {
          group = "gateway.k8s.aws"
          kind  = "LoadBalancerConfiguration"
          name  = local.gateway_name
        }
      }
      listeners = [{
        name     = "https"
        protocol = "HTTPS"
        port     = 443
        allowedRoutes = {
          namespaces = {
            from = "Selector"
            selector = {
              matchExpressions = [{
                key      = "kubernetes.io/metadata.name"
                operator = "In"
                values   = local.gateway_route_namespaces
              }]
            }
          }
        }
      }]
    }
  })
  wait = true

  depends_on = [
    kubectl_manifest.gateway_class,
    kubectl_manifest.gateway_lb_config,
    module.vpc, # public subnets + IGW route must outlive the ALB on destroy
  ]
}
