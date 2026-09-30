terraform {
  required_version = ">= 1.6"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 5.70" }
    random = { source = "hashicorp/random", version = "~> 3.6" }
  }
  # Remote state with locking; create the bucket/table once per account.
  backend "s3" {
    key          = "fleetpulse/terraform.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = {
      Project     = "fleetpulse"
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}
