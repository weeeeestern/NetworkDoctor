# ---------------------------------------------------------------------------
# external-dns — reads HTTPRoute hostnames and writes alias records for the
# Gateway ALB into the envs/dns zone, so a recreated ALB needs no manual DNS.
# TXT ownership records (txtOwnerId) keep it from touching other records.
# ---------------------------------------------------------------------------

data "aws_route53_zone" "lab" {
  name         = var.domain
  private_zone = false
}

data "aws_iam_policy_document" "external_dns" {
  statement {
    actions   = ["route53:ChangeResourceRecordSets"]
    resources = [data.aws_route53_zone.lab.arn]
  }

  statement {
    actions = [
      "route53:ListHostedZones",
      "route53:ListResourceRecordSets",
      "route53:ListTagsForResource",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_policy" "external_dns" {
  name   = "${var.name}-external-dns"
  policy = data.aws_iam_policy_document.external_dns.json
}

resource "aws_iam_role" "external_dns" {
  name               = "${var.name}-external-dns"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy_attachment" "external_dns" {
  role       = aws_iam_role.external_dns.name
  policy_arn = aws_iam_policy.external_dns.arn
}

resource "aws_eks_pod_identity_association" "external_dns" {
  cluster_name    = module.eks.cluster_name
  namespace       = "kube-system"
  service_account = "external-dns"
  role_arn        = aws_iam_role.external_dns.arn
}

resource "helm_release" "external_dns" {
  name       = "external-dns"
  repository = "https://kubernetes-sigs.github.io/external-dns/"
  chart      = "external-dns"
  version    = "1.23.0"
  namespace  = "kube-system"

  values = [yamlencode({
    provider      = { name = "aws" }
    sources       = ["gateway-httproute"]
    domainFilters = [var.domain]
    txtOwnerId    = var.name
    # Keep the apex alias ownership TXT record inside the public zone.
    txtPrefix = "nd-%%{record_type}."
    policy    = "sync" # remove records when the HTTPRoute goes away
    extraArgs = ["--aws-zone-type=public", "--zone-id-filter=${data.aws_route53_zone.lab.zone_id}"]
    env = [{
      name  = "AWS_DEFAULT_REGION"
      value = var.region
    }]
    serviceAccount = {
      name = "external-dns"
    }
  })]

  depends_on = [
    kubectl_manifest.gateway_api_crds, # the gateway-httproute source needs them at start
    aws_eks_pod_identity_association.external_dns,
    aws_iam_role_policy_attachment.external_dns,
    helm_release.cilium,
  ]
}
