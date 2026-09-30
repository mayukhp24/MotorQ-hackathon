data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  name = "fleetpulse-${var.environment}"
  azs  = slice(data.aws_availability_zones.available.names, 0, 3)
}

# Three AZs; data services live in isolated database subnets with no route to
# the internet, workloads in private subnets behind NAT, load balancers public.
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.13"

  name             = local.name
  cidr             = var.vpc_cidr
  azs              = local.azs
  private_subnets  = [for i, _ in local.azs : cidrsubnet(var.vpc_cidr, 4, i)]
  public_subnets   = [for i, _ in local.azs : cidrsubnet(var.vpc_cidr, 8, 48 + i)]
  database_subnets = [for i, _ in local.azs : cidrsubnet(var.vpc_cidr, 8, 64 + i)]

  enable_nat_gateway     = true
  one_nat_gateway_per_az = true
  enable_dns_hostnames   = true

  create_database_subnet_group       = true
  create_database_subnet_route_table = true

  enable_flow_log                      = true
  create_flow_log_cloudwatch_log_group = true
  create_flow_log_cloudwatch_iam_role  = true
  flow_log_max_aggregation_interval    = 60

  public_subnet_tags  = { "kubernetes.io/role/elb" = 1 }
  private_subnet_tags = { "kubernetes.io/role/internal-elb" = 1 }
}

# Data-service security group: reachable only from EKS nodes.
resource "aws_security_group" "data" {
  name        = "${local.name}-data"
  description = "Managed data services, reachable from EKS workloads only"
  vpc_id      = module.vpc.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "data_from_nodes" {
  for_each = {
    postgres = 5432
    redis    = 6379
    kafka    = 9096
  }
  security_group_id            = aws_security_group.data.id
  referenced_security_group_id = module.eks.node_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = each.value
  to_port                      = each.value
  description                  = "${each.key} from EKS nodes"
}

# Keep S3 and Secrets Manager traffic inside the VPC.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = module.vpc.vpc_id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = module.vpc.private_route_table_ids
}
