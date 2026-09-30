# Chaos experiments

Run at 2026-09-30 20:12 UTC against the docker compose stack.

| Experiment | Fault | Steady-state hypothesis | Result |
|---|---|---|---|
| Baseline | none | critical alert visible < 5 s | PASS |
| Stream-processor crash | SIGKILL one of two stream-processor replicas | partitions fail over to the survivor; the alert is still raised; lag drains | PASS |
| Kafka broker outage | stop the (single) Kafka broker for 20 s, then start it | gateway back-pressures instead of losing data; alert sent during the outage arrives after recovery | PASS |
| ClickHouse outage | stop ClickHouse for 30 s | operational reads (Postgres/Redis) unaffected; history/analytics answer fast with degraded=true; recover | PASS |
| Redis restart (state loss) | restart Redis (no persistence: live state, cache, limits lost) | API keeps serving; live state rebuilds from the stream; an alert raised after restart is delivered | PASS |

## Baseline

- alert latency 0.54 s

## Stream-processor crash

- killed motorq-hackathon-stream-processor-1 (owned partitions [1, 2, 5, 6, 9, 10]); probe vehicle is on partition 9
- alert delivered after 10.9 s (includes consumer-group rebalance)
- replica restarted; lag 0 after 8 s

## Kafka broker outage

- kafka stopped
- API reads during outage: /fleet/summary -> 200
- ingest call during outage -> 202
- alert delivered 30.6 s after send (outage 20 s + broker start)

## ClickHouse outage

- /fleet/summary -> 200, /vehicles/{vin} -> 200, /analytics/fleet-hourly -> 200 degraded=True in 0.16 s total
- telemetry history during outage: 200 degraded=True (0.00s), 200 degraded=True (0.00s), 200 degraded=True (0.00s)
- analytics recovered 0.3 s after ClickHouse came back

## Redis restart (state loss)

- redis restarted (empty)
- /vehicles after restart -> [200, 200, 200, 200, 200]
- alert after Redis restart delivered in 0.0 s
- vehicles online (live state) 15 s later: 55000
