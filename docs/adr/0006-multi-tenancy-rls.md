# ADR 0006: Shared-schema multi-tenancy with row-level security

**Status:** accepted

## Context
Three competing fleet operators on one platform; a data leak between them is
the highest-impact failure. Tenants range from 15K to 55K vehicles.

## Options considered
| Option | For | Against |
|---|---|---|
| Database per tenant | Strongest isolation | Migrations × N, connection pools × N, cross-tenant platform views hard |
| Schema per tenant | Good isolation | Same operational multiplication; ClickHouse and Kafka still shared |
| **Shared schema + RLS** | One migration path; isolation enforced by the database, not only by application code | Every table carries `tenant_id`; must guard materialised views |

## Decision
Shared schema. Every tenant table has `tenant_id` and an RLS policy
`tenant_id = app_tenant()`; the API role has no `BYPASSRLS` and sets the GUC per
transaction from the JWT claim. Materialised views are exposed only through
`security_barrier` views; ClickHouse uses a row policy on `SQL_tenant_id`.

## Consequences
* Application bugs (a missing `WHERE`) cannot leak across tenants.
* Security-barrier views block join push-down: handled with a fenced
  LATERAL probe (see `docs/sql-optimization.md`, 351 → 1.3 ms).
* A noisy tenant can still consume shared capacity: per-user rate limits
  today; per-tenant quotas are a future enhancement.
