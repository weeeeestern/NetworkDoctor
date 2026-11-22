provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "networkdoctor"
      Env       = "bootstrap"
      ManagedBy = "terraform"
    }
  }
}
