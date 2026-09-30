# ADR 0004: Go for the data path, Python for the API and ML

**Status:** accepted

## Context
The pipeline must process ~100K events/s with bounded memory and predictable
latency; the API and analytics need fast iteration, a rich ML ecosystem and
an official Claude SDK.

## Options considered
| Option | For | Against |
|---|---|---|
| **Go (pipeline) + Python (API, ML)** | Go: low-overhead concurrency, static binaries on distroless images, ~100–130K events/s per core measured. Python: FastAPI, scikit-learn, pandas, Anthropic SDK | Two toolchains; shared contracts must be explicit |
| All Python | One language | GIL and per-event overhead would need 10–20× the cores on the hot path |
| All Go | One language | ML and data-science tooling far weaker |
| JVM (Kafka Streams / Flink) | Mature stream processing | Heavier runtime and ops for a team of this size; state management would be similar in practice |

## Decision
Go for simulator, gateway, stream processor, sinks and migrations; Python for
the Fleet API and analytics; TypeScript/React for the UI.

## Consequences
* Contracts between them are JSON Schemas in `/contracts`, verified in CI from
  both sides (ADR 0005).
* The Python API is the throughput bottleneck per core (~100 req/s per 4 vCPU
  alongside the full stack); it is stateless and scales horizontally.
