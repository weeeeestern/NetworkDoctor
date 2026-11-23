locals {
  # Keep the profile in sync with deploy/helm/holmesgpt/values-eks-lab.yaml.
  holmes_model_id    = "anthropic.claude-haiku-4-5-20251001-v1:0"
  holmes_profile_arn = "arn:aws:bedrock:${var.region}:${data.aws_caller_identity.current.account_id}:inference-profile/global.${local.holmes_model_id}"
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
# regionless model separately. Only allow the underlying model via this profile.
data "aws_iam_policy_document" "holmes_bedrock" {
  statement {
    actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
    resources = [local.holmes_profile_arn]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = [var.region]
    }
  }

  statement {
    actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
    resources = ["arn:aws:bedrock:${var.region}::foundation-model/${local.holmes_model_id}"]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = [var.region]
    }

    condition {
      test     = "StringEquals"
      variable = "bedrock:InferenceProfileArn"
      values   = [local.holmes_profile_arn]
    }
  }

  statement {
    actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
    resources = ["arn:aws:bedrock:::foundation-model/${local.holmes_model_id}"]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = ["unspecified"]
    }

    condition {
      test     = "StringEquals"
      variable = "bedrock:InferenceProfileArn"
      values   = [local.holmes_profile_arn]
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
