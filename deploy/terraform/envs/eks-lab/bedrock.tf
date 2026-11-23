# ---------------------------------------------------------------------------
# Bedrock for the in-cluster LLM gateway (LiteLLM, namespace llm-gateway).
# Calls stay inside the VPC through the bedrock-runtime interface endpoint;
# the gateway's service account gets Bedrock access through Pod Identity, so
# no provider API key reaches the cluster.
# ---------------------------------------------------------------------------

locals {
  llm_gateway_namespace       = "llm-gateway"
  llm_gateway_service_account = "llm-gateway"

  # Models the gateway may call, through their global. cross-region inference
  # profiles. Keep in sync with deploy/helm/llm-gateway/values.yaml (models).
  # GPT-6 Luna matches the on-prem Holmes model; Claude Haiku 4.5 is the
  # fallback. Access and quotas for both were checked in Seoul (#38).
  bedrock_model_ids = {
    luna  = "openai.gpt-6-luna"
    haiku = "anthropic.claude-haiku-4-5-20251001-v1:0"
  }
  # Global inference authorizes the source profile, the regional model and
  # the regionless model separately (#38).
  bedrock_model_arns = {
    for name, model_id in local.bedrock_model_ids : name => {
      profile    = "arn:aws:bedrock:${var.region}:${data.aws_caller_identity.current.account_id}:inference-profile/global.${model_id}"
      regional   = "arn:aws:bedrock:${var.region}::foundation-model/${model_id}"
      regionless = "arn:aws:bedrock:::foundation-model/${model_id}"
    }
  }
  # Luna requests also authorize against the account's default project.
  bedrock_default_project_arn = "arn:aws:bedrock:${var.region}:${data.aws_caller_identity.current.account_id}:project/default"
  bedrock_invoke_actions      = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
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

# Only the gateway role, only the two models. The role policy below ties
# each underlying model to its own profile.
data "aws_iam_policy_document" "bedrock_endpoint" {
  statement {
    principals {
      type        = "AWS"
      identifiers = ["*"]
    }
    actions = local.bedrock_invoke_actions
    resources = concat([local.bedrock_default_project_arn], flatten([
      for model in local.bedrock_model_arns : [model.profile, model.regional, model.regionless]
    ]))

    condition {
      test     = "ArnEquals"
      variable = "aws:PrincipalArn"
      values   = [aws_iam_role.llm_gateway.arn]
    }
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
    actions   = local.bedrock_invoke_actions
    resources = concat([local.bedrock_default_project_arn], [for model in local.bedrock_model_arns : model.profile])

    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = [var.region]
    }
  }

  dynamic "statement" {
    for_each = local.bedrock_model_arns

    content {
      actions   = local.bedrock_invoke_actions
      resources = [statement.value.regional]

      condition {
        test     = "StringEquals"
        variable = "aws:RequestedRegion"
        values   = [var.region]
      }

      condition {
        test     = "StringEquals"
        variable = "bedrock:InferenceProfileArn"
        values   = [statement.value.profile]
      }
    }
  }

  dynamic "statement" {
    for_each = local.bedrock_model_arns

    content {
      actions   = local.bedrock_invoke_actions
      resources = [statement.value.regionless]

      condition {
        test     = "StringEquals"
        variable = "aws:RequestedRegion"
        values   = ["unspecified"]
      }

      condition {
        test     = "StringEquals"
        variable = "bedrock:InferenceProfileArn"
        values   = [statement.value.profile]
      }
    }
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
