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

# Used to install Cilium and Argo CD. The token comes
# from the local AWS credentials via `aws eks get-token`, so no static
# kubeconfig is needed.
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
