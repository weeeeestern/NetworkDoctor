# ---------------------------------------------------------------------------
# Long-lived DNS for the EKS lab: the public Route53 zone and one wildcard
# ACM certificate. Kept out of envs/eks-lab so destroying the lab does not
# change the zone's name servers or re-issue the certificate.
#
# First apply creates the zone; copy the `name_servers` output into
# My가비아 → 도메인 관리 → 네임서버 설정. ACM validation finishes only after
# that delegation is visible (see deploy/terraform/README.md, step 0).
# ---------------------------------------------------------------------------

resource "aws_route53_zone" "this" {
  name = var.domain
}

# Covers the NetworkDoctor apex and the lab subdomains.
resource "aws_acm_certificate" "this" {
  domain_name               = var.domain
  subject_alternative_names = ["*.${var.domain}"]
  validation_method         = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

# Apex and wildcard share one validation record; allow_overwrite lets the
# duplicated for_each entries converge instead of conflicting.
resource "aws_route53_record" "acm_validation" {
  for_each = {
    for dvo in aws_acm_certificate.this.domain_validation_options : dvo.domain_name => {
      name   = dvo.resource_record_name
      type   = dvo.resource_record_type
      record = dvo.resource_record_value
    }
  }

  zone_id         = aws_route53_zone.this.zone_id
  name            = each.value.name
  type            = each.value.type
  ttl             = 60
  records         = [each.value.record]
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "this" {
  certificate_arn         = aws_acm_certificate.this.arn
  validation_record_fqdns = [for r in aws_route53_record.acm_validation : r.fqdn]

  # Waits for the registrar delegation. Apply this only after
  # `dig NS <domain>` shows the Route53 servers; a longer wait would outlive
  # the GitHub Actions job timeout and leave the state lock behind.
  timeouts {
    create = "60m"
  }
}
