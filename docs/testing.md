# Test strategy and evidence

| Test type | Tools | Tests | Result | In CI |
|---|---|---|---|---|
| Unit – Go pipeline | `go test -race` | 84 tests + 6 benchmarks | pass; **84.3%** statement coverage of pipeline logic (gate 80%; excludes DB/Kafka wiring exercised end to end) | yes |
| Unit – Fleet API | pytest, fakeredis, httpx mock transport | 63 | pass | yes |
| Unit – analytics | pytest | 10 | pass | yes |
| Unit – web | Vitest, Testing Library | 17 | pass | yes |
| Integration – API | pytest + **Testcontainers** (PostgreSQL 16 + pgvector with every migration, Redis) | 17 (80 API tests in total) | pass; API coverage **88%** (gate 80%) | yes |
| Integration – analytics jobs | pytest + Testcontainers (real PostgreSQL, production writer role) | 6 | pass; analytics coverage **90%** (gate 80%) | yes |
| Contract | Go: producer structs vs JSON Schemas; ClickHouse DDL covers every contract field | 2 suites (3,000 telemetry, 689 alerts, 321 trips validated per run) | pass | yes |
| Acceptance (BDD) | behave against the running stack | 3 features, 9 scenarios, 32 steps | 9/9 pass; overheat alert visible via API 0.15–0.33 s after the OEM sends it | yes (`e2e` job) |
| Load | k6 (`tests/load/api.js`) | 100 req/s × 5 min, 12 users, 3 tenants | 30,025 requests, p95 **135 ms**, p99 **408 ms**, 0 errors | smoke profile in CI |
| Rate limiting | k6 (`tests/load/ratelimit.js`) | noisy tenant vs neighbour | 429 + `Retry-After` for the noisy user; neighbour unaffected | manual |
| Soak | k6 `PROFILE=soak` (2 h at 100 req/s) | — | script provided; not run in the build window | manual |
| Chaos | `tests/chaos/chaos.py` (Docker faults) | 5 experiments | 5/5 pass (see below) | manual |
| Security | gitleaks, Semgrep, govulncheck, npm audit, Trivy (fs, IaC, images), ZAP baseline | — | configured in CI (`security`, `images`, `e2e` jobs); not executed in the build sandbox | yes |
| IaC | helm lint, kubeconform (4 value sets), terraform fmt/validate | — | chart valid for default/AWS/GCP/KEDA; Terraform formatted (validate runs in CI; registry blocked in the sandbox) | yes |

Reports: `docs/evidence/` (k6 summaries and per-endpoint latency, chaos report,
EXPLAIN plans, pipeline benchmarks). CI uploads coverage XML, JUnit, SBOMs and
the BDD report as artifacts.

## Edge cases covered

| Edge case | Where tested |
|---|---|
| Duplicate events (OEM retries), within and across transports | `TestProcess_DedupEnrichAndAlert`, `TestDedup_WindowRotation`, simulator injects 1% duplicates |
| Out-of-order and late events | `TestLateEventsIgnoredForState`, `TestLateEventNotWrittenToLiveState`, simulator delays 2% by 1–10 s |
| Malformed JSON, bad VIN check digit, impossible readings, clock skew | `TestHTTP_AcceptsValidatesAndDLQs`, `TestHTTP_Unparseable`, `TestValidate_Rejections`, BDD "Malformed records are rejected without losing the valid ones" |
| Unknown OEM, unknown vehicle | `TestHandle_UnknownOEM`, `TestProcess_UnknownVehicleAndGarbage` |
| OEM format change / new OEM without code change | `TestHotOnboardNewOEM`, `TestSpecValidation`, `TestMapping_TypeErrors` |
| Unit conversions (mph, °F, psi, Kelvin, m/s) | `TestPinnacle_UnitConversion`, `TestStellar`, `TestEncoders_RoundTripThroughAdapters` |
| Gzip payloads | `TestHTTP_Gzip` |
| Kafka down / back-pressure | `TestHTTP_BackPressure`, `TestHTTP_SinkErrorIs503`, chaos "Kafka broker outage" |
| Processor crash mid-stream | chaos "Stream-processor crash" (probe vehicle on the killed replica's partitions) |
| ClickHouse down | `test_clickhouse_outage_degrades_gracefully`, breaker tests, chaos "ClickHouse outage" |
| Redis down / wiped | `test_rate_limiter_fails_open_on_redis_error`, chaos "Redis restart" |
| Cross-tenant access | `test_tenant_isolation_by_rls`, BDD tenant scenarios |
| JWT forgery (alg none, foreign key, wrong audience/issuer, expiry) | `tests/unit/test_security.py` |
| Cursor tampering, deep pagination | `test_cursor_roundtrip_and_tamper`, `test_keyset_pagination_walks_all_rows_once` |
| Concurrent edits | `test_work_order_optimistic_concurrency` |
| Audit log tampering | `test_audit_tamper_detection` |
| Erasure twice | `test_erasure_flow_and_audit_chain` (second request → 409) |
| Prompt injection, ungrounded VINs, duplicate AI proposals | `test_guardrail_*`, `test_ungrounded_vins`, `test_copilot_offline_*` |
| LLM provider quirks: missing tool-call IDs, invalid JSON arguments, tool calls written as text (`<function=…>`, bare JSON), leftover special tokens, empty or endless replies and rate limit / outage → offline fallback, write tools hidden from viewers | `tests/unit/test_copilot_providers.py` (mocked OpenAI-compatible server) |
| Label leakage in ML | `test_labels_look_forward_only`, time-split evaluation |
| EV cannot fail on cooling/misfire | `test_ev_cannot_fail_on_cooling`, integration scoring test |
| Simulator rate cap larger than one refill | `TestRunner_RateLimitSustainsTarget` (regression) |

## Chaos results (`docs/evidence/chaos-report.md`)

| Fault | Result |
|---|---|
| none (baseline) | sentinel critical alert visible via API in 0.02–0.54 s |
| SIGKILL one of two processors (probe on its partitions) | alert delivered after 10.9 s (session timeout 10 s + rebalance; was 45 s before tuning); lag drained in 8 s after restart |
| Kafka stopped for 20 s | gateway accepted the batch (202) and delivered after recovery: alert 30.6 s after send; API reads unaffected |
| ClickHouse stopped 30 s | operational reads 200; history/analytics 200 with `degraded: true` in < 10 ms; recovered 0.3 s after restart |
| Redis wiped | API 200 throughout; live state for 55,000 vehicles back within 15 s; alerts still delivered |

## Sample input / output

`contracts/oem-samples/` holds one generated payload per OEM format (nested
metric JSON, flat imperial JSON, signal lists in SI units).
`scripts/send-sample.py` re-stamps them to the current time and posts them:

```
$ scripts/send-sample.py
aurora    202 {"accepted":2,"rejected":0}
pinnacle  202 {"accepted":2,"rejected":0}
stellar   202 {"accepted":2,"rejected":0}
$ scripts/send-sample.py pinnacle --corrupt
pinnacle  202 {"accepted":1,"rejected":1}
```

The rejected record goes to `telemetry.dlq.v1` with the raw payload and the
reason, e.g. `invalid event: vin "PNCTRH5D1JD000019": vin: check digit mismatch:
got 1 want X`; the valid record in the same batch is processed normally. Events
older than the retention window or more than the allowed clock skew in the
future are rejected the same way.

## Running the suites

```bash
# Go (unit + contract + race detector)
cd services/pipeline && go test -race ./...
# API (unit + Testcontainers integration; needs Docker)
cd services/fleet-api && pip install -e ".[test]" && pytest --cov=app
# Analytics
cd services/analytics && pip install -e ".[test]" && pytest --cov=analytics
# Web
cd services/web && npm ci && npm test
# Against a running stack (docker compose up -d):
cd tests/bdd && behave features
k6 run -e BASE_URL=http://localhost:8000 tests/load/api.js
python tests/chaos/chaos.py
scripts/explain-evidence.sh
```
