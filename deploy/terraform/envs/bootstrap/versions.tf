terraform {
  required_version = ">= 1.10" # S3-native state locking in the stacks it serves

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.80"
    }
  }
}
