# ---------------------------------------------------------------------------
# Route53 + ACM for the Gabia-registered domain (var.domain).
#
# The domain stays registered at Gabia; only DNS is delegated here. After the
# first apply, copy the 4 name servers from the `route53_name_servers` output
# into My가비아 → 도메인 관리 → 네임서버 설정 → "타사 네임서버".
# ACM validation waits (up to ~75m) until that delegation propagates.
# ---------------------------------------------------------------------------

resource "aws_route53_zone" "this" {
  name = var.domain
}

resource "aws_acm_certificate" "this" {
  domain_name               = var.domain
  subject_alternative_names = ["*.${var.domain}"] # grafana.netdoc.cloud 등 서브도메인 커버
  validation_method         = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

# Apex and wildcard resolve to the same validation record; allow_overwrite
# lets the duplicated for_each entries converge instead of conflicting.
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
}
