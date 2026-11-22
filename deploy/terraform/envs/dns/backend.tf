# Remote state in the bucket envs/bootstrap creates. The bucket name includes
# the account ID, so it is passed at init time instead of committed:
#
#   terraform init -backend-config="bucket=$(terraform -chdir=../bootstrap output -raw state_bucket)"
#
# GitHub Actions passes it from the TF_STATE_BUCKET repository variable.
terraform {
  backend "s3" {
    key          = "networkdoctor/dns/terraform.tfstate"
    region       = "ap-northeast-2"
    encrypt      = true
    use_lockfile = true # S3-native locking (Terraform >= 1.10), no DynamoDB table
  }
}
