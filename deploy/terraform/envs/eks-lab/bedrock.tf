# ---------------------------------------------------------------------------
# Bedrock interface endpoint for the EKS lab. HolmesGPT uses the dedicated
# Pod Identity role in holmes-bedrock.tf. An in-cluster LLM gateway can also
# use the existing llm-gateway role.
# ---------------------------------------------------------------------------

locals {
  llm_gateway_namespace       = "llm-gateway"
  llm_gateway_service_account = "llm-gateway"

  # Claude models, called directly or through apac./global. cross-region
  # inference profiles (the profile and the models it routes to).
  bedrock_model_arns = [
    "arn:aws:bedrock:*::foundation-model/anthropic.*",
    "arn:aws:bedrock:*:${data.aws_caller_identity.current.account_id}:inference-profile/*",
  ]
}

data "aws_caller_identity" "current" {}

resource "aws_security_group" "bedrock_endpoint" {
  name_prefix = "${var.name}-bedrock-vpce-"
  description = "HTTPS from EKS workers to the Bedrock runtime endpoint"
  vpc_id      = module.vpc.vpc_id

  ingress {
    description     = "HTTPS from worker nodes and their pods"
    from_port       = 443
    to_port         = 443
    protocol        = "tcp"
    security_groups = [module.eks.node_security_group_id]
  }

  lifecycle {
    create_before_destroy = true
  }
}

data "aws_iam_policy_document" "bedrock_endpoint" {
  statement {
    principals {
      type        = "AWS"
      identifiers = ["*"]
    }
    actions = [
      "bedrock:InvokeModel",
      "bedrock:InvokeModelWithResponseStream",
    ]
    resources = concat(local.bedrock_model_arns, [
      local.holmes_default_project_arn,
      local.holmes_regional_model_arn,
      local.holmes_global_model_arn,
    ])
  }
}

# Private DNS makes the regional bedrock-runtime hostname resolve to the
# endpoint, so the gateway needs no endpoint-specific URL.
resource "aws_vpc_endpoint" "bedrock_runtime" {
  vpc_id              = module.vpc.vpc_id
  service_name        = "com.amazonaws.${var.region}.bedrock-runtime"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = module.vpc.private_subnets
  security_group_ids  = [aws_security_group.bedrock_endpoint.id]
  private_dns_enabled = true
  policy              = data.aws_iam_policy_document.bedrock_endpoint.json
}

data "aws_iam_policy_document" "llm_gateway" {
  statement {
    actions = [
      "bedrock:InvokeModel",
      "bedrock:InvokeModelWithResponseStream",
    ]
    resources = local.bedrock_model_arns
  }
}

resource "aws_iam_policy" "llm_gateway" {
  name   = "${var.name}-llm-gateway-bedrock"
  policy = data.aws_iam_policy_document.llm_gateway.json
}

resource "aws_iam_role" "llm_gateway" {
  name               = "${var.name}-llm-gateway"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy_attachment" "llm_gateway" {
  role       = aws_iam_role.llm_gateway.name
  policy_arn = aws_iam_policy.llm_gateway.arn
}

# The association may exist before the namespace and service account; the
# LiteLLM chart creates them later.
resource "aws_eks_pod_identity_association" "llm_gateway" {
  cluster_name    = module.eks.cluster_name
  namespace       = local.llm_gateway_namespace
  service_account = local.llm_gateway_service_account
  role_arn        = aws_iam_role.llm_gateway.arn
}
