# Remote state. Same bucket as envs/eks-lab works; only the key differs.
#
# terraform {
#   backend "s3" {
#     bucket       = "<your-tfstate-bucket>"
#     key          = "networkdoctor/dns/terraform.tfstate"
#     region       = "ap-northeast-2"
#     use_lockfile = true # S3-native locking (Terraform >= 1.10); use a DynamoDB table on older versions
#   }
# }
