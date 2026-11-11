# ---------------------------------------------------------------------------
# external-dns — watches HTTPRoute hostnames and writes the matching A/ALIAS
# records into the Route53 zone, so the Gateway-created ALB gets
# https://<domain> automatically (no manual record after each recreate).
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "external_dns" {
  statement {
    actions   = ["route53:ChangeResourceRecordSets"]
    resources = [aws_route53_zone.this.arn]
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
  version    = "1.15.0"
  namespace  = "kube-system"

  values = [yamlencode({
    provider = { name = "aws" }
    # Gateway API HTTPRoute hostnames are the record source. NOTE: the pod
    # crashloops until the Gateway API CRDs are installed (README step 2).
    sources       = ["gateway-httproute"]
    domainFilters = [var.domain]
    txtOwnerId    = var.name # TXT registry: marks records this instance owns
    policy        = "sync"   # remove records when the HTTPRoute goes away
    serviceAccount = {
      name = "external-dns"
    }
  })]

  depends_on = [
    module.eks,
    aws_eks_pod_identity_association.external_dns,
    helm_release.cilium, # start already Cilium-managed (no restart needed)
  ]
}
