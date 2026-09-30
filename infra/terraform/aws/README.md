# FleetPulse on AWS (Terraform)

Provisions everything the Helm chart expects to find: a 3-AZ VPC, EKS (with
network-policy enforcement, KMS-encrypted secrets, IMDSv2-only nodes), RDS
PostgreSQL 16 (Multi-AZ, TLS-only, PITR), ElastiCache Redis (TLS + AUTH,
failover), Amazon MSK (SASL/SCRAM over TLS, RF 3), an S3 cold tier for
ClickHouse, KMS keys per data class, and every application secret in Secrets
Manager (read by External Secrets Operator through IRSA).

```bash
cd infra/terraform/aws
cp terraform.tfvars.example terraform.tfvars
terraform init -backend-config="bucket=<state-bucket>" -backend-config="region=eu-west-1"
terraform apply
$(terraform output -raw kubeconfig_command)
terraform output -json helm_values     # endpoints for values-aws.yaml
```

Then install cluster add-ons (ingress-nginx, cert-manager, External Secrets
Operator with the `external_secrets_role_arn`, optionally KEDA and the
Altinity ClickHouse operator using the `olap` node group and S3 cold tier),
put the three out-of-band secrets (`jwt-private-key-pem`, `ingest-api-keys`,
`anthropic-api-key`) into Secrets Manager, and deploy:

```bash
helm upgrade --install fleetpulse deploy/helm/fleetpulse -n fleetpulse --create-namespace \
  -f deploy/helm/fleetpulse/values-aws.yaml
```

Estimated cost at the default sizing is in `docs/capacity.md`. `terraform
destroy` is blocked by RDS deletion protection on purpose.
