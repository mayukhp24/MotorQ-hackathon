# ADR 0001: Kafka as the event backbone

**Status:** accepted

## Context
100K events/s sustained, 3× bursts, out-of-order and duplicate delivery from
OEM clouds, several independent consumers (stream processing, ClickHouse,
PostgreSQL sinks), and the need to replay history after a bug fix.

## Options considered
| Option | For | Against |
|---|---|---|
| **Apache Kafka** | Partitioned, replicated log; per-key ordering; consumer groups; replay by offset; mature managed offerings (MSK, Confluent, GCP Managed Kafka); ClickHouse reads it natively | Operational weight; partitions fix max parallelism |
| RabbitMQ | Flexible routing, simple ops | Messages deleted on ack (no replay); ordering per queue only; lower throughput per node for this shape |
| MQTT broker only | Devices already speak it | Not a durable log; no replay; no consumer-group scaling |
| Cloud-native streams (Kinesis, Pub/Sub) | Fully managed | Ties the design to one cloud (the brief requires cloud-agnostic); ClickHouse integration via extra glue |

## Decision
Kafka for everything after the edge. MQTT (with shared subscriptions) remains
the device-facing protocol and is bridged into Kafka by the ingest gateway.
Topics: `telemetry.v1` (key VIN, 48 partitions in production), enriched,
alerts, trips, DLQ; producers idempotent with `acks=all`; RF 3,
`min.insync.replicas=2`.

## Consequences
* Per-VIN ordering makes per-vehicle state machines correct without locks.
* A consumer bug is fixed by resetting offsets and replaying 24 h.
* Burst absorption: producers keep writing while consumers scale (KEDA on lag).
* Partition count caps processor parallelism: sized at 48 for ~6× headroom.
* Chaos test: a 20 s broker outage lost nothing (gateway buffered and retried).
