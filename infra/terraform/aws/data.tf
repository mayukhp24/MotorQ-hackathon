# ---------------------------------------------------------------- PostgreSQL
# System of record (3NF, RLS, pgvector). Multi-AZ, encrypted, TLS enforced,
# 14-day PITR, Performance Insights, deletion protection.
resource "aws_db_parameter_group" "pg" {
  name   = "${local.name}-pg16"
  family = "postgres16"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
  parameter {
    name         = "shared_preload_libraries"
    value        = "pg_stat_statements"
    apply_method = "pending-reboot"
  }
  parameter {
    name  = "log_min_duration_statement"
    value = "500"
  }
  parameter {
    name  = "random_page_cost"
    value = "1.1"
  }
}

resource "aws_db_instance" "pg" {
  identifier     = local.name
  engine         = "postgres"
  engine_version = "16.4"
  instance_class = var.postgres_instance_class

  allocated_storage     = 200
  max_allocated_storage = 2000
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = aws_kms_key.this["data"].arn

  db_name                             = "fleetpulse"
  username                            = "fleetpulse_admin"
  password                            = random_password.app["db-migrate-password"].result
  port                                = 5432
  multi_az                            = true
  db_subnet_group_name                = module.vpc.database_subnet_group_name
  vpc_security_group_ids              = [aws_security_group.data.id]
  parameter_group_name                = aws_db_parameter_group.pg.name
  publicly_accessible                 = false
  iam_database_authentication_enabled = true

  backup_retention_period         = 14
  backup_window                   = "02:00-03:00"
  maintenance_window              = "sun:03:30-sun:04:30"
  copy_tags_to_snapshot           = true
  deletion_protection             = true
  skip_final_snapshot             = false
  final_snapshot_identifier       = "${local.name}-final"
  performance_insights_enabled    = true
  performance_insights_kms_key_id = aws_kms_key.this["data"].arn
  enabled_cloudwatch_logs_exports = ["postgresql"]
  auto_minor_version_upgrade      = true
}

# ---------------------------------------------------------------- Redis
# Live vehicle state, KPI snapshots, rate limits, cache. TLS + AUTH, Multi-AZ
# with automatic failover; state is rebuildable from Kafka so no AOF.
resource "aws_elasticache_subnet_group" "redis" {
  name       = local.name
  subnet_ids = module.vpc.database_subnets
}

resource "aws_elasticache_replication_group" "redis" {
  replication_group_id       = local.name
  description                = "FleetPulse live state and cache"
  engine                     = "redis"
  engine_version             = "7.1"
  node_type                  = var.redis_node_type
  num_cache_clusters         = 2
  automatic_failover_enabled = true
  multi_az_enabled           = true
  subnet_group_name          = aws_elasticache_subnet_group.redis.name
  security_group_ids         = [aws_security_group.data.id]
  at_rest_encryption_enabled = true
  kms_key_id                 = aws_kms_key.this["data"].arn
  transit_encryption_enabled = true
  auth_token                 = random_password.app["redis-password"].result
  snapshot_retention_limit   = 1
  parameter_group_name       = "default.redis7"
}

# ---------------------------------------------------------------- Kafka (MSK)
# Event backbone. 3 brokers across AZs, RF=3/min ISR 2, SASL/SCRAM over TLS,
# encrypted at rest; 72 h retention allows replay after a consumer bug.
resource "aws_msk_configuration" "kafka" {
  name              = local.name
  kafka_versions    = ["3.7.x"]
  server_properties = <<-PROPS
    auto.create.topics.enable=false
    default.replication.factor=3
    min.insync.replicas=2
    num.partitions=48
    log.retention.hours=72
    compression.type=producer
    unclean.leader.election.enable=false
  PROPS
}

resource "aws_msk_cluster" "kafka" {
  cluster_name           = local.name
  kafka_version          = "3.7.x"
  number_of_broker_nodes = var.kafka_broker_count

  broker_node_group_info {
    instance_type   = var.kafka_instance_type
    client_subnets  = module.vpc.private_subnets
    security_groups = [aws_security_group.data.id]
    storage_info {
      ebs_storage_info {
        volume_size = 2000
      }
    }
  }

  configuration_info {
    arn      = aws_msk_configuration.kafka.arn
    revision = aws_msk_configuration.kafka.latest_revision
  }

  client_authentication {
    sasl {
      scram = true
    }
  }

  encryption_info {
    encryption_at_rest_kms_key_arn = aws_kms_key.this["data"].arn
    encryption_in_transit {
      client_broker = "TLS"
      in_cluster    = true
    }
  }

  open_monitoring {
    prometheus {
      jmx_exporter {
        enabled_in_broker = true
      }
      node_exporter {
        enabled_in_broker = true
      }
    }
  }
}

# MSK SCRAM secrets must be AmazonMSK_-prefixed and encrypted with a CMK.
resource "aws_secretsmanager_secret" "msk_scram" {
  name       = "AmazonMSK_${local.name}"
  kms_key_id = aws_kms_key.this["secrets"].arn
}

resource "aws_secretsmanager_secret_version" "msk_scram" {
  secret_id = aws_secretsmanager_secret.msk_scram.id
  secret_string = jsonencode({
    username = "fleetpulse"
    password = random_password.app["kafka-sasl-password"].result
  })
}

resource "aws_msk_scram_secret_association" "kafka" {
  cluster_arn     = aws_msk_cluster.kafka.arn
  secret_arn_list = [aws_secretsmanager_secret.msk_scram.arn]
  depends_on      = [aws_secretsmanager_secret_version.msk_scram]
}

# ---------------------------------------------------------------- S3 cold tier
# ClickHouse moves telemetry parts older than the hot window here (S3 disk);
# model artefacts and backups share the bucket under separate prefixes.
resource "aws_s3_bucket" "cold" {
  bucket_prefix = "${local.name}-cold-"
}

resource "aws_s3_bucket_public_access_block" "cold" {
  bucket                  = aws_s3_bucket.cold.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "cold" {
  bucket = aws_s3_bucket.cold.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.this["data"].arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_versioning" "cold" {
  bucket = aws_s3_bucket.cold.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "cold" {
  bucket = aws_s3_bucket.cold.id
  rule {
    id     = "telemetry-archive"
    status = "Enabled"
    filter {
      prefix = "clickhouse/"
    }
    transition {
      days          = 90
      storage_class = "GLACIER_IR"
    }
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }
}

data "aws_iam_policy_document" "cold_tls_only" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.cold.arn, "${aws_s3_bucket.cold.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "cold" {
  bucket = aws_s3_bucket.cold.id
  policy = data.aws_iam_policy_document.cold_tls_only.json
}
