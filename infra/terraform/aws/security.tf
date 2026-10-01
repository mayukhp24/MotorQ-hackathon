# One customer-managed key per data class (AES-256 at rest), yearly rotation.
resource "aws_kms_key" "this" {
  for_each                = toset(["data", "secrets", "logs"])
  description             = "fleetpulse ${var.environment} ${each.key}"
  enable_key_rotation     = true
  deletion_window_in_days = 30
}

resource "aws_kms_alias" "this" {
  for_each      = aws_kms_key.this
  name          = "alias/${local.name}-${each.key}"
  target_key_id = each.value.key_id
}

# Application credentials: generated here, stored only in Secrets Manager,
# synced into the cluster by External Secrets Operator. URL-safe characters so
# they can be embedded in connection strings.
locals {
  generated_secrets = [
    "db-api-password", "db-writer-password", "db-migrate-password", "redis-password",
    "kafka-sasl-password", "clickhouse-password", "clickhouse-ro-password",
    "pii-encryption-key", "cursor-secret", "mqtt-gateway-password",
  ]
  # Supplied out of band (never in state): JWT signing key, OEM API keys, LLM keys
  # (put any non-empty placeholder in the LLM key you do not use).
  external_secrets = ["jwt-private-key-pem", "ingest-api-keys", "llm-api-key", "anthropic-api-key"]
}

resource "random_password" "app" {
  for_each = toset(local.generated_secrets)
  length   = 40
  special  = false
}

resource "aws_secretsmanager_secret" "app" {
  for_each                = toset(concat(local.generated_secrets, local.external_secrets, ["kafka-sasl-username"]))
  name                    = "fleetpulse/${var.environment}/${each.key}"
  kms_key_id              = aws_kms_key.this["secrets"].arn
  recovery_window_in_days = 7
}

resource "aws_secretsmanager_secret_version" "generated" {
  for_each      = random_password.app
  secret_id     = aws_secretsmanager_secret.app[each.key].id
  secret_string = each.value.result
}

resource "aws_secretsmanager_secret_version" "kafka_user" {
  secret_id     = aws_secretsmanager_secret.app["kafka-sasl-username"].id
  secret_string = "fleetpulse"
}

# IRSA role for External Secrets Operator: read-only on this app's secrets.
data "aws_iam_policy_document" "eso" {
  statement {
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
    resources = [for s in aws_secretsmanager_secret.app : s.arn]
  }
  statement {
    actions   = ["kms:Decrypt"]
    resources = [aws_kms_key.this["secrets"].arn]
  }
}

module "eso_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts-eks"
  version = "~> 5.46"

  role_name = "${local.name}-external-secrets"
  role_policy_arns = {
    read = aws_iam_policy.eso.arn
  }
  oidc_providers = {
    main = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["external-secrets:external-secrets"]
    }
  }
}

resource "aws_iam_policy" "eso" {
  name   = "${local.name}-external-secrets"
  policy = data.aws_iam_policy_document.eso.json
}
