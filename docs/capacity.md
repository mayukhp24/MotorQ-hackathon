# Capacity and retention plan

Sizing basis: **100,000 vehicles reporting at 1 Hz = 100,000 events/s
sustained**, with a 3× burst (300,000 events/s) for 5 minutes at shift start.
Per-record sizes are measured on the running stack, not assumed.

| Measurement | Value | Source |
|---|---|---|
| Canonical event, JSON | 407 B | average of `telemetry.v1` messages |
| Kafka on disk (lz4, batched) | 124 B/record | log-dir size ÷ offsets |
| ClickHouse `telemetry` row | 31.3 B compressed (154 B raw, 4.9×) | `system.parts` |
| ClickHouse `vehicle_daily` row | 63 B | `system.parts` |
| PostgreSQL `alert` / `trip` / `risk_score` / `audit_log` | 429 / 414 / 317 / 480 B per row incl. indexes | `pg_total_relation_size` |

## Throughput per stage (per core)

| Stage | Capacity | Cores for 100K ev/s (sustained, 60% target utilisation) | Cores for 300K ev/s burst |
|---|---|---|---|
| Gateway decode + validate | ~100K records/s/core | 2 (+ Kafka client overhead: plan 3 pods × 2 vCPU) | 6 → HPA to 9 pods |
| Stream processor | ~116–134K events/s/core (in-memory path) | 2 (+ Kafka/Redis I/O: plan 6 pods × 2 vCPU) | 48 partitions cap parallelism; KEDA scales on lag |
| ClickHouse insert | ~1M rows/s per replica with 64K-row blocks (vendor guidance; our Kafka engine uses 8 consumers) | 1 shard × 2 replicas | burst absorbed by Kafka |

Numbers from `docs/evidence/pipeline-benchmarks.txt`. The 4-vCPU sandbox
sustained ~4K events/s end to end with every component (Kafka, ClickHouse,
PostgreSQL, Redis, simulator, API, Grafana) sharing the same four cores; the
per-stage benchmarks are the basis for extrapolation, and the chart's HPA /
KEDA bounds are set from this table.

## Kafka

| Topic | Rate on disk | Retention | Replicas | Storage |
|---|---|---|---|---|
| `telemetry.v1` (key VIN, 48 partitions) | 12.4 MB/s | 24 h (replay window) | 3 | 3.2 TB |
| `telemetry.enriched.v1` | ~16 MB/s (≈1.3× raw) | 6 h (only feeds ClickHouse) | 3 | 1.0 TB |
| `alerts.v1`, `trips.v1`, `telemetry.dlq.v1` | < 0.1 MB/s | 7 d | 3 | < 0.1 TB |
| **Total** | | | | **≈ 4.3 TB → 3 brokers × 3 TB** (≈ 2× headroom) |

A 5-minute 3× burst adds ~7 GB per replica; the constraint during a burst is
consumer throughput, not disk, and Kafka simply buffers until KEDA has added
processors.

## ClickHouse (telemetry)

| Tier | Window | Volume |
|---|---|---|
| Hot (NVMe) | 7 days | 8.64 B rows/day × 31.3 B = **270 GB/day** → 1.9 TB (3.8 TB with 2 replicas) |
| Cold (S3 disk, `TTL … TO VOLUME 'cold'`) | days 8–90 | 22.4 TB |
| Deleted | > 90 days raw | rollups remain |
| Rollups (`vehicle_daily`, `fleet_hourly`) | 2 years / 400 days | ~6 MB/day → < 5 GB |

Partitioned by day, ordered by `(tenant_id, vin, ts, seq)`: a single vehicle's
6-hour history reads 13 of 8,386 granules (`docs/evidence/explain/07-clickhouse-pk.txt`).

## PostgreSQL

| Table | Rows/day at 100K vehicles | Growth/year | Retention |
|---|---|---|---|
| `trip` (monthly partitions) | ~300K | ~45 GB | 13 months, then `DROP PARTITION` |
| `alert` | ~18K | ~2.8 GB | 2 years (resolved alerts are also in ClickHouse `alerts_history`) |
| `risk_score` | 100K | ~11.6 GB | 90 days of history; current score in the read model |
| `audit_log` | 1 row per authenticated request (≈860K/day at 10 req/s average) | ~150 GB | 1 year hot; see risk below |
| Reference data (vehicles, drivers, users) | — | < 1 GB | life of contract; drivers erasable |

Enforcement today: Kafka and ClickHouse retention are enforced by topic
config and table TTLs. In PostgreSQL, future `trip` partitions are created
automatically (`ensure_trip_partitions`), while dropping expired partitions
and pruning `risk_score` history are policies still to be automated (a small
CronJob; tracked in the risks section of the solution document).

`db.r7g.xlarge` (32 GB RAM) keeps the hot working set (current risk, open
alerts, reference data, recent trips) in memory.

**Known risk:** `audit_log` is not partitioned yet. At sustained API traffic it
becomes the largest table; the next step is monthly partitions with the hash
chain carried across partition boundaries, and archiving closed months to S3
Object Lock (WORM).

## Redis

Live state is ~600 B per vehicle (hash) + GEO member: ~70 MB for 100K
vehicles; KPI snapshots, rate-limit buckets and caches add < 100 MB.
`cache.r7g.large` (13 GB) with a replica leaves ample headroom. Redis holds no
durable data: the chaos run wiped it and live state for all 55,000 online
vehicles of a tenant was back within 15 s.

## Retention summary (hot / warm / cold)

| Data | Hot | Warm | Cold / archive | Deleted |
|---|---|---|---|---|
| Raw telemetry | Kafka 24 h; ClickHouse NVMe 7 d | — | ClickHouse S3 to 90 d | > 90 d |
| Aggregates / features | ClickHouse 2 y | — | — | > 2 y |
| Alerts, work orders | PostgreSQL | — | ClickHouse `alerts_history` | 2 y |
| Trips | PostgreSQL 13 months | — | — | > 13 months |
| Audit log | PostgreSQL 1 y | — | S3 Object Lock 7 y (planned) | per policy |
| Driver PII | encrypted until erasure | — | — | on request (erasure API) |
