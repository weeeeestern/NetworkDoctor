locals {
  # Keep this port in sync with deploy/helm/networkdoctor/values-eks-lab.yaml.
  team_backend_node_port = 30080
}

resource "aws_security_group" "team_alb" {
  name_prefix = "${var.name}-team-alb-"
  description = "Team HTTPS to the NetworkDoctor demo"
  vpc_id      = module.vpc.vpc_id

  ingress {
    description = "HTTPS from team IPs"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = var.team_api_cidrs
  }

  egress {
    description     = "Backend NodePort and health checks"
    from_port       = local.team_backend_node_port
    to_port         = local.team_backend_node_port
    protocol        = "tcp"
    security_groups = [module.eks.node_security_group_id]
  }
}

resource "aws_security_group_rule" "team_alb_to_workers" {
  description              = "Backend NodePort from the team ALB"
  type                     = "ingress"
  from_port                = local.team_backend_node_port
  to_port                  = local.team_backend_node_port
  protocol                 = "tcp"
  security_group_id        = module.eks.node_security_group_id
  source_security_group_id = aws_security_group.team_alb.id
}

resource "aws_lb" "team" {
  # Use a short AWS-generated name because ALB names are limited to 32 chars.
  name_prefix                = "nd-"
  load_balancer_type         = "application"
  internal                   = false
  ip_address_type            = "ipv4"
  security_groups            = [aws_security_group.team_alb.id]
  subnets                    = module.vpc.public_subnets
  drop_invalid_header_fields = true

  depends_on = [module.vpc]
}

resource "aws_lb_target_group" "team_backend" {
  name_prefix = "nd-"
  vpc_id      = module.vpc.vpc_id
  target_type = "instance"
  protocol    = "HTTP"
  port        = local.team_backend_node_port

  health_check {
    path    = "/healthz"
    matcher = "200"
  }
}

# Register instances as EKS replaces them. Leave ASG health checks at the EKS
# default, so an unhealthy demo app does not trigger worker replacement.
resource "aws_autoscaling_attachment" "team_backend" {
  autoscaling_group_name = module.workers.node_group_autoscaling_group_names[0]
  lb_target_group_arn    = aws_lb_target_group.team_backend.arn
}

resource "aws_lb_listener" "team_https" {
  load_balancer_arn = aws_lb.team.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.team_alb_certificate_arn

  default_action {
    type = "fixed-response"
    fixed_response {
      content_type = "text/plain"
      status_code  = "403"
      message_body = "Use the operator connection for changes."
    }
  }

  lifecycle {
    precondition {
      condition     = split(":", var.team_alb_certificate_arn)[3] == var.region
      error_message = "Use an ACM certificate ARN from the same AWS region as the ALB."
    }
  }
}

resource "aws_lb_listener_rule" "team_read" {
  listener_arn = aws_lb_listener.team_https.arn

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.team_backend.arn
  }

  condition {
    http_request_method {
      values = ["GET", "HEAD"]
    }
  }
}
