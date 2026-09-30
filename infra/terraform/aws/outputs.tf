output "cluster_name" {
  value = module.eks.cluster_name
}

output "kubeconfig_command" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

# Paste into deploy/helm/fleetpulse/values-aws.yaml (or pass with -f).
output "helm_values" {
  value = {
    endpoints = {
      kafkaBrokers = aws_msk_cluster.kafka.bootstrap_brokers_sasl_scram
      postgres     = { host = aws_db_instance.pg.address, port = aws_db_instance.pg.port }
      redis        = { host = aws_elasticache_replication_group.redis.primary_endpoint_address, port = 6379, tls = true }
    }
    serviceAccount = { annotations = { "eks.amazonaws.com/role-arn" = module.workloads_irsa.iam_role_arn } }
    networkPolicy  = { dataServiceCIDRs = [var.vpc_cidr] }
    coldTierBucket = aws_s3_bucket.cold.bucket
  }
}

output "external_secrets_role_arn" {
  value = module.eso_irsa.iam_role_arn
}
