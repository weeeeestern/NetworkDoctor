locals {
  # Keep these model IDs in sync with deploy/helm/holmesgpt/values-eks-lab.yaml.
  holmes_model_ids = {
    luna  = "openai.gpt-6-luna"
    haiku = "anthropic.claude-haiku-4-5-20251001-v1:0"
  }
  holmes_model_arns = {
    for name, model_id in local.holmes_model_ids : name => {
      profile    = "arn:aws:bedrock:${var.region}:${data.aws_caller_identity.current.account_id}:inference-profile/global.${model_id}"
      regional   = "arn:aws:bedrock:${var.region}::foundation-model/${model_id}"
      regionless = "arn:aws:bedrock:::foundation-model/${model_id}"
    }
  }
  holmes_default_project_arn = "arn:aws:bedrock:${var.region}:${data.aws_caller_identity.current.account_id}:project/default"
  holmes_invoke_actions      = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
}

data "aws_iam_policy_document" "holmes_assume" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]

    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/eks-cluster-arn"
      values   = [module.eks.cluster_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/kubernetes-namespace"
      values   = ["holmesgpt"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/kubernetes-service-account"
      values   = ["holmes-bedrock"]
    }
  }
}

resource "aws_iam_role" "holmes_bedrock" {
  name               = "${var.name}-holmes-bedrock"
  assume_role_policy = data.aws_iam_policy_document.holmes_assume.json
}

# Global inference authorizes the source profile, regional model, and
# regionless model separately. The source statement also covers the default
# project used by Luna. Tie each underlying model to its own profile.
data "aws_iam_policy_document" "holmes_bedrock" {
  statement {
    actions   = local.holmes_invoke_actions
    resources = concat([local.holmes_default_project_arn], [for model in local.holmes_model_arns : model.profile])

    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = [var.region]
    }
  }

  dynamic "statement" {
    for_each = local.holmes_model_arns

    content {
      actions   = local.holmes_invoke_actions
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
    for_each = local.holmes_model_arns

    content {
      actions   = local.holmes_invoke_actions
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

resource "aws_iam_role_policy" "holmes_bedrock" {
  name   = "invoke-demo-model"
  role   = aws_iam_role.holmes_bedrock.name
  policy = data.aws_iam_policy_document.holmes_bedrock.json
}

resource "aws_eks_pod_identity_association" "holmes_bedrock" {
  cluster_name    = module.eks.cluster_name
  namespace       = "holmesgpt"
  service_account = "holmes-bedrock"
  role_arn        = aws_iam_role.holmes_bedrock.arn

  depends_on = [
    aws_eks_addon.workers["eks-pod-identity-agent"],
    aws_iam_role_policy.holmes_bedrock,
    aws_vpc_endpoint.bedrock_runtime,
  ]
}
