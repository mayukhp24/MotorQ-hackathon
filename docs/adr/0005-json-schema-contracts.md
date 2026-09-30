# ADR 0005: JSON Schema event contracts with consumer checks in CI

**Status:** accepted

## Context
Five topics cross language boundaries (Go producers, ClickHouse and Python
consumers). OEM formats change without notice. A silent schema change is the
most likely way to corrupt analytics.

## Options considered
| Option | For | Against |
|---|---|---|
| Avro / Protobuf + schema registry | Compact, enforced compatibility | Extra infrastructure; ClickHouse and browser tooling need converters; harder to debug |
| **JSON + JSON Schema in the repo** | Human-readable, native to ClickHouse's JSONEachRow and every language; schemas versioned in Git with the code | Larger on the wire (mitigated by lz4: 124 B/record on disk) |

## Decision
JSON on the wire, one JSON Schema per topic in `/contracts`, topic names
versioned (`telemetry.v1`). Contract tests serialise real producer structs and
validate them against the schemas, and check that the ClickHouse consumer DDL
maps every required field. OEM formats are handled by declarative adapter
specs (`internal/oem/specs/*.json`), so a format change is a spec change, and
unknown or malformed records go to a DLQ instead of failing the batch.

## Consequences
* Breaking changes fail CI before deployment.
* Additive changes are free; breaking ones need a `.v2` topic and dual-write.
* A registry can be introduced later without changing producers' structs.
