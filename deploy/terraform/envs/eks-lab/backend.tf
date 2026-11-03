# Remote state. Create the bucket once, then uncomment and `terraform init`.
#
#   aws s3api create-bucket --bucket <your-tfstate-bucket> \
#     --region ap-northeast-2 \
#     --create-bucket-configuration LocationConstraint=ap-northeast-2
#   aws s3api put-bucket-versioning --bucket <your-tfstate-bucket> \
#     --versioning-configuration Status=Enabled
#
# terraform {
#   backend "s3" {
#     bucket       = "<your-tfstate-bucket>"
#     key          = "networkdoctor/eks-lab/terraform.tfstate"
#     region       = "ap-northeast-2"
#     use_lockfile = true # S3-native locking (Terraform >= 1.10); use a DynamoDB table on older versions
#   }
# }
