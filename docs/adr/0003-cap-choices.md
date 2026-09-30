# ADR 0003: Consistency choices per store (CAP)

**Status:** accepted

## Context
During a network partition each store must either refuse writes
(consistency) or accept them and reconcile later (availability). The right
answer differs by data: losing an alert or double-booking a work order is
worse than a stale map.

## Decision
| Data | Choice | Mechanism | Why |
|---|---|---|---|
| Event log (Kafka) | **CP** | RF 3, `min.insync.replicas=2`, `acks=all`, unclean leader election off | Telemetry is the source of truth; producers retry and back-pressure rather than write to a minority |
| Work orders, users, tenants, alerts (PostgreSQL) | **CP** | Synchronous commit, Multi-AZ failover, optimistic locking (`work_order.version`) | Money and safety decisions; no split brain |
| Live state, KPI snapshots, rate limits (Redis) | **AP** | Async replica, no persistence | Rebuilt from the stream within seconds (chaos test: 55,000 vehicles back in 15 s); a slightly stale position is acceptable |
| Telemetry history (ClickHouse) | **AP-leaning** | Async replication; `ReplacingMergeTree` converges duplicates | Analytics tolerate seconds of lag and eventual dedup |

## Consequences
* Ingest can pause (503/back-pressure) during a Kafka quorum loss instead of
  silently losing data.
* Dashboards stay up during a Redis failover with brief staleness.
* The API must treat ClickHouse results as eventually consistent (e.g. counts
  may briefly include duplicates before merges), which is acceptable for
  history charts; operational counts come from PostgreSQL.
