# FleetPulse architecture

FleetPulse ingests telemetry from 100,000 connected vehicles reported by three
OEM clouds, detects faults in real time, predicts breakdowns seven days ahead
and serves three competing fleet operators from one multi-tenant platform.

## 1. System context (C4 level 1)

```mermaid
flowchart LR
  subgraph Users
    FM[Fleet manager / maintenance lead]
    AN[Analyst / viewer]
    PO[Platform operator]
  end
  subgraph OEMs["OEM clouds (3 formats)"]
    A[Aurora Motors<br/>nested JSON, metric, epoch ms]
    P[Pinnacle Auto<br/>flat JSON, imperial, ISO-8601]
    S[Stellar EV<br/>signal list, Kelvin, m/s]
  end
  FP((FleetPulse))
  IDP[Identity provider<br/>OIDC, optional]
  LLM[Claude API<br/>copilot reasoning]
  SM[Cloud secret manager]
  A -- MQTT/TLS (mTLS) --> FP
  P -- HTTPS push (API key + client cert) --> FP
  S -- MQTT/TLS (mTLS) --> FP
  FM & AN & PO -- HTTPS / WSS --> FP
  FP -- OIDC / JWKS --> IDP
  FP -- HTTPS tool-use loop --> LLM
  FP -- IRSA / workload identity --> SM
```

## 2. Containers (C4 level 2)

```mermaid
flowchart LR
  OEM[OEM clouds / simulator] -->|MQTT QoS1<br/>shared subscription| MQ[(Mosquitto / EMQX)]
  OEM -->|HTTPS batch POST| GW
  MQ -->|MQTT| GW[ingest-gateway<br/>Go]
  GW -->|Kafka telemetry.v1<br/>key = VIN| K[(Kafka)]
  GW -->|Kafka telemetry.dlq.v1| K
  K -->|consumer group| SP[stream-processor<br/>Go, N replicas]
  SP -->|telemetry.enriched.v1| K
  SP -->|alerts.v1, trips.v1| K
  SP -->|HASH / GEO / pub-sub| R[(Redis<br/>live state)]
  K -->|Kafka engine| CH[(ClickHouse<br/>telemetry OLAP)]
  K -->|consumer group| SW[sink-writer<br/>Go] -->|COPY / upsert| PG[(PostgreSQL 16<br/>+ pgvector)]
  AJ[analytics jobs<br/>Python] -->|features SQL, Parquet| CH
  AJ -->|risk scores, KB embeddings| PG
  API[fleet-api<br/>FastAPI] -->|asyncpg, RLS| PG
  API -->|HTTP, row policy| CH
  API -->|live state, rate limits, cache| R
  API -->|Messages API + tools| LLM[Claude API]
  WEB[web<br/>React + nginx] -->|HTTPS / WSS /api| API
  PROM[Prometheus] -.->|scrape /metrics| GW & SP & SW & API & AJ
  GRAF[Grafana] -.-> PROM
```

| Container | Responsibility | Scales on |
|---|---|---|
| `ingest-gateway` | Authenticate OEM connectors, decode 3 formats through declarative adapters, validate (VIN check digit, ranges, clock skew), canonicalise, produce to Kafka; malformed records to the DLQ; back-pressure (bounded in-flight, 429/503) | CPU (HPA) |
| `stream-processor` | Per-partition state: Bloom-filter dedup, out-of-order handling, 12 detection rules, trip segmentation, idle cost, live state to Redis, KPI snapshots, top-K fault codes | Consumer lag (KEDA) / CPU |
| `sink-writer` | Alerts and trips from Kafka to PostgreSQL (idempotent, keyed by deterministic IDs) | Partitions of `alerts.v1`/`trips.v1` |
| ClickHouse Kafka engine | Enriched telemetry to `fleet.telemetry` (ReplacingMergeTree) and rollup MVs | Kafka engine consumers |
| `fleet-api` | OAuth2/JWT, RBAC, tenant isolation, keyset pagination, rate limits, WebSocket stream, copilot, compliance | CPU (HPA), stateless |
| `analytics` | Feature extraction (ClickHouse), training/evaluation, daily scoring, knowledge-base embeddings, read-model refresh | CronJobs |
| `web` | SPA served by nginx; same-origin proxy to the API | Replicas |

## 3. Life of one telemetry event

Measured on the docker compose stack (4 vCPU) with 100K vehicles reporting
at ~4K events/s; benchmark figures from `docs/evidence/pipeline-benchmarks.txt`.

| # | Hop | Mechanism | Latency |
|---|---|---|---|
| 1 | Vehicle → OEM cloud → FleetPulse edge | OEM batches (≤ 500 records) over MQTT QoS 1 or HTTPS | network-bound (outside our control) |
| 2 | Gateway decode + validate | Adapter per OEM, VIN check digit, range checks | ~10 µs/record (≈100K records/s/core) |
| 3 | Gateway → Kafka | idempotent producer, acks=all, lz4, linger 5 ms | 5–15 ms |
| 4 | Kafka → stream processor | consumer group, per-VIN ordering by key | 1–10 ms |
| 5 | Processing | dedup, rules, trips, enrichment | ~8 µs/event (≈116–134K events/s/core) |
| 6 | Alert → API / WebSocket | Redis pub/sub (instant) and `alerts.v1` → sink-writer → PostgreSQL | **p95 480 ms** (Grafana), sentinel alert visible via API in 0.02–0.54 s |
| 7 | Live state → dashboard | Redis HASH/GEO flushed every 500 ms; KPI snapshot + WebSocket push every 1 s | per-vehicle ≤ ~0.8 s; KPI tiles ≤ ~2 s |
| 8 | Enriched → ClickHouse | Kafka engine + materialized views | seconds (history/analytics only) |
| 9 | Nightly | features → model → `risk_score` → read model | daily batch |

Ingest-to-processed p95 is 239 ms (`processor_ingest_to_process_seconds`).

### Critical flow: overheating vehicle to manager's screen

```mermaid
sequenceDiagram
  autonumber
  participant V as OEM cloud
  participant GW as Gateway
  participant K as Kafka
  participant SP as Processor
  participant R as Redis
  participant API as API
  participant UI as Browser
  participant PG as Postgres
  V->>GW: POST batch
  GW->>GW: decode, validate
  GW->>K: telemetry.v1 (key VIN)
  K-->>GW: ack (all ISR)
  GW-->>V: 202
  K->>SP: poll
  SP->>SP: dedup, 15 s hold
  SP->>K: alerts.v1
  SP->>R: PUBLISH
  R-->>API: pub/sub
  API-->>UI: WebSocket alert
  K->>PG: sink-writer upsert
  Note over V,PG: measured 0.02–0.54 s to API, p95 480 ms (target < 5 s)
```

### Failure path: Kafka unavailable, OEM retries, duplicates

```mermaid
sequenceDiagram
  autonumber
  participant O as OEM cloud
  participant GW as Gateway
  participant K as Kafka
  participant SP as Processor
  participant PG as Postgres
  Note over K: broker down (20 s)
  O->>GW: POST batch A
  GW->>K: produce (buffered)
  Note over GW: held until acked
  O->>GW: POST batch B
  GW-->>O: 429 Retry-After 1
  Note over K: broker back
  K-->>GW: ack batch A
  GW-->>O: 202 (batch A)
  alt delivery timed out
    GW-->>O: 503, OEM retries
  end
  O->>GW: retry batch B (same seq)
  GW->>K: produce
  K->>SP: A, B, retried copies
  SP->>SP: Bloom dedup by event_id
  SP->>K: alerts (deterministic IDs)
  K->>PG: upsert: replays are no-ops
  Note over O,PG: measured: alert sent in the outage arrived after 30.6 s, no loss
```

The gateway never acknowledges a batch before Kafka has it (`acks=all`); above
the producer's high-water mark it answers 429 with `Retry-After`, and if
delivery times out it answers 503. Either way the OEM retries, the retried
records carry the same `(OEM, VIN, sequence)` and therefore the same
`event_id`, the processor's Bloom filter drops them, and any alert derived
twice has the same deterministic ID, so the PostgreSQL upsert is a no-op.
Rendered diagrams: `docs/diagrams/*.png`.

## 4. Data architecture (polyglot persistence)

| Store | Data | Why this store | CAP choice |
|---|---|---|---|
| **Kafka** | Raw, enriched, alerts, trips, DLQ | Durable, replayable log; ordering per VIN; decouples ingest from processing | CP (RF 3, `min.insync.replicas=2`, `acks=all`, no unclean election) |
| **ClickHouse** | 1 Hz telemetry, daily features, hourly rollups, reject log | Columnar compression (31 B/row vs 407 B JSON), primary-key pruning on `(tenant_id, vin, ts)`, sub-second scans over billions of rows | AP-leaning (async replication, ReplacingMergeTree converges duplicates) |
| **PostgreSQL 16** | 3NF system of record: tenants, users, fleets, vehicles, drivers (PII encrypted), alerts, work orders, trips (partitioned), risk scores, audit chain, agent actions; `pgvector` knowledge base | Transactions, constraints, RLS for tenant isolation, joins for the operational UI | CP (synchronous commit, Multi-AZ failover) |
| **Redis** | Live vehicle state (HASH + GEO), KPI snapshots, pub/sub for alerts, rate-limit buckets, response cache, WebSocket tickets | Sub-millisecond reads for the live map; state is rebuildable from the stream | AP (a wipe loses nothing durable: verified in the chaos run) |

Details: [ERD](erd.md) · [capacity and retention](capacity.md) · [query optimisation](sql-optimization.md).

## 5. Deployment view

```mermaid
flowchart TB
  subgraph Cloud["Cloud account (AWS example: infra/terraform/aws)"]
    subgraph VPC["VPC, 3 AZs"]
      subgraph EKS["EKS (namespace fleetpulse)"]
        ING[ingress-nginx + cert-manager] --> WEBD[web x2]
        ING --> APID[fleet-api x3..20 HPA]
        ING -->|client-cert host| GWD[ingest-gateway x3..24 HPA]
        GWD --> SPD[stream-processor x6..48 KEDA]
        SWD[sink-writer x2]
        CJ[analytics CronJobs]
        CHO[ClickHouse operator<br/>olap node group]
        ESO[External Secrets Operator]
      end
      MSK[(MSK Kafka x3<br/>SASL/SCRAM + TLS)]
      RDS[(RDS PostgreSQL<br/>Multi-AZ)]
      EC[(ElastiCache Redis<br/>TLS + AUTH)]
    end
    S3[(S3 cold tier)]
    SMGR[Secrets Manager + KMS]
  end
  ESO --> SMGR
  CHO --> S3
```

* **Namespaces and policies:** one namespace per environment; a default-deny
  `NetworkPolicy` with explicit allows (ingress controller → web/API/gateway,
  pods → data-service CIDRs, `fleet-api` → HTTPS for the LLM API, Prometheus →
  metrics ports). Metadata endpoint blocked.
* **Pods:** non-root UID 10001, read-only root filesystem, all capabilities
  dropped, seccomp `RuntimeDefault`, zone and node spreading, PDBs,
  rolling updates with `maxUnavailable: 0` for stateless tiers.
* **Secrets:** generated by Terraform into Secrets Manager (KMS), synced by
  External Secrets Operator; nothing secret in Git, images or Helm history.
* **Migrations:** Helm hook Job (`post-install,pre-upgrade`) so new code never
  meets an old schema.

**Cloud-agnostic:** the chart only needs endpoints and a secret store, so the
same chart runs on GKE (`values-gcp.yaml`: Cloud SQL, Memorystore, Managed
Kafka, GCS) or locally (`docker compose up`). Everything in the data path is
open source (Kafka, ClickHouse, PostgreSQL, Redis, Mosquitto/EMQX); managed
services are substitutes, not dependencies.

## 6. Service internals (layering)

The Go pipeline and the API both follow ports and adapters: domain logic has
no I/O and is tested without infrastructure; adapters implement narrow
interfaces.

```mermaid
flowchart TB
  subgraph stream-processor
    CMD[cmd/stream-processor<br/>wiring, config, signals] --> APP[processor.Processor<br/>use case: process batch, flush, snapshot]
    APP --> DOM[detect.Engine, stream.Dedup, trip state machine<br/>pure domain logic]
    APP --> PORTS[ports: Meta, Output, Live]
    ADP[adapters.go: Kafka output, Redis live store,<br/>Postgres registry] -. implements .-> PORTS
  end
```

| Layer | FleetPulse example | Must not |
|---|---|---|
| Presentation / API | `services/fleet-api/app/api/*` (routing, validation, auth dependencies, problem+json) | Contain SQL or business rules |
| Application / service | `app/services/*`, `internal/processor`, `analytics/jobs.py` | Depend on a concrete broker/DB client type (ports: `Output`, `Live`, `Sink`) |
| Domain | `internal/detect`, `internal/stream`, `internal/vin`, `internal/dtc`, `analytics/model.py` | Import framework or infrastructure code |
| Infrastructure | `internal/platform`, `processor/adapters.go`, `app/infra/*` | Leak driver types upward |

Repository layout:

```
contracts/        JSON Schemas for every topic + one sample payload per OEM
db/               PostgreSQL migrations (3NF, RLS, partitions, read models), ClickHouse DDL
deploy/           Helm chart, Mosquitto, ClickHouse and observability config
docs/             architecture, ERD, ADRs, security, algorithms, evidence, screenshots
infra/terraform/  AWS (VPC, EKS, RDS, ElastiCache, MSK, S3, KMS, Secrets Manager)
scripts/          evidence capture (EXPLAIN before/after)
services/
  pipeline/       Go: simulator, ingest-gateway, stream-processor, sink-writer, migrate
  fleet-api/      Python FastAPI: REST, WebSocket, copilot, compliance
  analytics/      Python: features, predictive model, scoring, knowledge base
  web/            React + TypeScript + Tailwind, served by nginx
tests/            BDD (behave), load (k6), chaos
```
