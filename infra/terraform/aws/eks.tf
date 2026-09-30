module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.24"

  cluster_name    = local.name
  cluster_version = var.eks_version

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  cluster_endpoint_public_access       = length(var.admin_cidrs) > 0
  cluster_endpoint_public_access_cidrs = var.admin_cidrs
  cluster_endpoint_private_access      = true

  # Kubernetes Secrets envelope-encrypted with a customer-managed key.
  cluster_encryption_config = {
    resources        = ["secrets"]
    provider_key_arn = aws_kms_key.this["secrets"].arn
  }
  cluster_enabled_log_types              = ["api", "audit", "authenticator"]
  cloudwatch_log_group_kms_key_id        = aws_kms_key.this["logs"].arn
  cloudwatch_log_group_retention_in_days = 90

  enable_irsa                              = true
  enable_cluster_creator_admin_permissions = true

  cluster_addons = {
    coredns                = {}
    kube-proxy             = {}
    vpc-cni                = { before_compute = true, configuration_values = jsonencode({ enableNetworkPolicy = "true" }) }
    aws-ebs-csi-driver     = { service_account_role_arn = module.ebs_csi_irsa.iam_role_arn }
    eks-pod-identity-agent = {}
  }

  eks_managed_node_groups = {
    for name, ng in var.node_groups : name => {
      instance_types = ng.instance_types
      min_size       = ng.min
      desired_size   = ng.desired
      max_size       = ng.max
      labels         = ng.labels
      ami_type       = "AL2023_x86_64_STANDARD"
      # IMDSv2 only, one hop: pods cannot borrow the node's instance role.
      metadata_options = {
        http_endpoint               = "enabled"
        http_tokens                 = "required"
        http_put_response_hop_limit = 1
      }
      block_device_mappings = {
        xvda = {
          device_name = "/dev/xvda"
          ebs = {
            volume_size = 100
            volume_type = "gp3"
            encrypted   = true
            kms_key_id  = aws_kms_key.this["data"].arn
          }
        }
      }
      taints = name == "olap" ? {
        clickhouse = { key = "workload", value = "clickhouse", effect = "NO_SCHEDULE" }
      } : {}
    }
  }
}

module "ebs_csi_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts-eks"
  version = "~> 5.46"

  role_name             = "${local.name}-ebs-csi"
  attach_ebs_csi_policy = true
  ebs_csi_kms_cmk_ids   = [aws_kms_key.this["data"].arn]
  oidc_providers = {
    main = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["kube-system:ebs-csi-controller-sa"]
    }
  }
}

# Workload role (IRSA) for the fleetpulse service account: ClickHouse cold tier
# and model artefacts in S3, nothing else.
data "aws_iam_policy_document" "workloads" {
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket"]
    resources = [aws_s3_bucket.cold.arn, "${aws_s3_bucket.cold.arn}/*"]
  }
  statement {
    actions   = ["kms:Decrypt", "kms:GenerateDataKey"]
    resources = [aws_kms_key.this["data"].arn]
  }
}

resource "aws_iam_policy" "workloads" {
  name   = "${local.name}-workloads"
  policy = data.aws_iam_policy_document.workloads.json
}

module "workloads_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts-eks"
  version = "~> 5.46"

  role_name        = "${local.name}-workloads"
  role_policy_arns = { s3 = aws_iam_policy.workloads.arn }
  oidc_providers = {
    main = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["fleetpulse:fleetpulse", "clickhouse:clickhouse"]
    }
  }
}
