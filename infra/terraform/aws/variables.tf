variable "region" {
  type    = string
  default = "eu-west-1"
}

variable "environment" {
  type    = string
  default = "prod"
}

variable "vpc_cidr" {
  type    = string
  default = "10.40.0.0/16"
}

variable "eks_version" {
  type    = string
  default = "1.30"
}

# Sized for 100K vehicles at 1 Hz (100K ev/s sustained, 300K ev/s burst);
# see docs/capacity.md for the derivation.
variable "node_groups" {
  type = map(object({
    instance_types = list(string)
    min            = number
    desired        = number
    max            = number
    labels         = map(string)
  }))
  default = {
    stream = { instance_types = ["c7i.2xlarge"], min = 3, desired = 6, max = 18, labels = { workload = "stream" } }
    apps   = { instance_types = ["m7i.xlarge"], min = 3, desired = 3, max = 9, labels = { workload = "apps" } }
    olap   = { instance_types = ["r7i.2xlarge"], min = 3, desired = 3, max = 6, labels = { workload = "clickhouse" } }
  }
}

variable "postgres_instance_class" {
  type    = string
  default = "db.r7g.xlarge"
}

variable "redis_node_type" {
  type    = string
  default = "cache.r7g.large"
}

variable "kafka_instance_type" {
  type    = string
  default = "kafka.m7g.xlarge"
}

variable "kafka_broker_count" {
  type    = number
  default = 3
}

variable "admin_cidrs" {
  description = "CIDRs allowed to reach the EKS public API endpoint (empty = private only)."
  type        = list(string)
  default     = []
}
