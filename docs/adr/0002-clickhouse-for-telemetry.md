# ADR 0002: ClickHouse for telemetry, PostgreSQL for the system of record

**Status:** accepted

## Context
8.6 billion telemetry rows per day at full scale, queried as per-vehicle
histories, fleet roll-ups and ML feature windows; alongside a relational core
(tenants, users, vehicles, work orders, alerts) that needs constraints,
transactions and row-level security.

## Options considered
| Option | For | Against |
|---|---|---|
| **ClickHouse** | Columnar compression (31 B/row measured), sparse primary index on `(tenant, vin, ts)`, materialised roll-ups, native Kafka engine, S3 cold tier, open source | Eventual consistency, no row-level transactions, updates are expensive |
| TimescaleDB | PostgreSQL compatibility, one engine | Higher storage per row at this scale; heavier write path for 100K rows/s on one primary |
| Cassandra / ScyllaDB | Linear write scaling | Query-first modelling, weak ad-hoc analytics, needs a second system for aggregation anyway |
| Everything in PostgreSQL | Simplicity | 3 TB/day raw; vacuum and index maintenance would dominate |

## Decision
Telemetry and analytical roll-ups in ClickHouse (`ReplacingMergeTree` to absorb
at-least-once duplicates, `AggregatingMergeTree` roll-ups, TTL to S3 then
delete). PostgreSQL 16 for the 3NF core, RLS, audit chain and `pgvector`.
Redis for live state.

## Consequences
* A single vehicle's 6 h history reads 13 of 8,386 granules.
* Two query languages and two tenant-isolation mechanisms (RLS and ClickHouse
  row policies), both tested.
* The API degrades gracefully when ClickHouse is down (`degraded: true`),
  verified in the chaos run; operational screens keep working from
  PostgreSQL and Redis.
