provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "networkdoctor"
      Env       = "eks-lab"
      ManagedBy = "terraform"
    }
  }
}

# Helm installs Cilium, the AWS Load Balancer Controller, external-dns and
# Argo CD; kubectl applies the Gateway API CRDs and the lab Gateway. Both get
# their token from the local AWS credentials via `aws eks get-token`, so no
# static kubeconfig is needed.
provider "helm" {
  kubernetes {
    host                   = module.eks.cluster_endpoint
    cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)

    exec {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      args        = ["eks", "get-token", "--cluster-name", module.eks.cluster_name, "--region", var.region]
    }
  }
}

provider "kubectl" {
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)
  load_config_file       = false

  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", module.eks.cluster_name, "--region", var.region]
  }
}
