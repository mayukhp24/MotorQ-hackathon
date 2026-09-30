# Query optimisation (EXPLAIN ANALYZE before / after)

All plans were captured on the seeded dataset (100,000 vehicles, 2.0M trips,
536K alerts, 1.1M risk scores, 74M telemetry rows) by
`scripts/explain-evidence.sh`. Queries run **as the least-privilege API role
with the tenant GUC set**, so row-level security and security-barrier views
apply exactly as in production. Full plans: `docs/evidence/explain/`.

| # | Query (tenant: Acme Logistics, 55K vehicles) | Before (ms) | After (ms) | Change made |
|---|---|---:|---:|---|
| 1 | Vehicle list page (50 rows + current risk) | 351.1 | 1.3 | Fenced `LEFT JOIN LATERAL (… OFFSET 0)` probe of the risk view instead of a plain join |
| 2 | Page 1,000 of the vehicle list | 40.5 | 1.2 | Keyset pagination (`vin > :cursor`) instead of `OFFSET 50000` |
| 3 | Riskiest vehicles (top 50, risk ≥ 0.2) | 2,910.7 | 7.3 | Materialised current-risk read model instead of `DISTINCT ON` over score history |
| 4 | Driver safety ranking, 30 days | 2,009.7 | 36.5 | `mv_driver_safety` read model refreshed hourly |
| 5 | Alert trend, 14 days by severity | 1,001.1 | 0.5 | `mv_alert_daily` for completed days |
| 6 | Open-alerts feed (newest 50) | 0.41 | 0.31 | Partial index `(tenant_id, ts DESC) WHERE status = 'OPEN'`: 2 MB vs 40 MB for the full index |
| 7 | ClickHouse: one vehicle, last 6 h | 8,386 / 8,386 granules | 13 / 8,386 granules | Filter on the sort-key prefix `(tenant_id, vin, ts)`; never wrap key columns in functions |

## 1. The security-barrier join trap (351 ms → 1.3 ms)

Materialised views cannot carry RLS, so tenants see `mv_vehicle_risk_current`
through a `security_barrier` view. A barrier view is never flattened into the
outer query, and a join condition cannot be pushed into it, so

```sql
FROM vehicle v … LEFT JOIN vehicle_risk_current r ON r.vin = v.vin
WHERE v.tenant_id = $1 ORDER BY v.vin LIMIT 51
```

scans all 55,000 risk rows of the tenant and sorts them to merge-join 51
vehicles:

```
Merge Left Join (actual time=390.7..391.3 rows=51)
  ->  Sort  Sort Key: r.vin  (quicksort, 4717kB)
        ->  Subquery Scan on r (rows=55000)
              ->  Index Scan using mv_vehicle_risk_current_tenant_risk (rows=55000)
Execution Time: 392 ms
```

A plain `LATERAL` did not help: the planner pulled it back up into the same
join. `OFFSET 0` is an optimisation fence, so the lateral subquery stays a
parameterised probe and the (leak-proof) `vin =` predicate reaches the unique
index:

```
Nested Loop Left Join (actual time=0.24..1.35 rows=51)
  ->  Index Scan using vehicle_tenant_vin on vehicle v (rows=51)
  ->  Subquery Scan on rc (loops=51)
        ->  Index Scan using mv_vehicle_risk_current_vin (rows=1 loops=51)
Execution Time: 1.7 ms
```

With a `risk_min` filter the join is kept: the `(tenant_id, risk_7d DESC)`
index makes the filtered set small, and probing every vehicle would be slower.
Code: `services/fleet-api/app/services/fleet.py` (`RISK_PROBE`,
`RISK_FILTER_JOIN`). Under load this moved `/vehicles` from 330–500 ms to
20–50 ms end to end.

## 2. Keyset instead of OFFSET (40.5 ms → 1.2 ms)

`OFFSET n` reads and discards n rows, so cost grows linearly with page depth.
Every list endpoint uses an opaque, HMAC-signed cursor carrying the last sort
key; the next page is an index range scan starting after it, constant cost at
any depth, and stable when rows are inserted concurrently.

## 3–5. CQRS read models (1–3 s → 0.5–36 ms)

Dashboards ask the same aggregate questions thousands of times between data
changes. The latest score per vehicle, 30-day driver safety and daily alert
counts are materialised and refreshed `CONCURRENTLY` (unique index present) by
the analytics jobs after scoring and hourly. Today's alerts are counted live
and added to the completed-day counts, so the trend is still current.

## 6. Partial index for the open-alerts feed

94% of alerts are resolved. The partial index covers only open alerts: 2 MB
instead of 40 MB. At this data size both fit in memory and the latency gain is
small (0.41 → 0.31 ms); the benefit is cache footprint and write cost as the
alert history grows, which is why it is kept.

## 7. ClickHouse primary-key pruning

`fleet.telemetry` is `ORDER BY (tenant_id, vin, ts, seq)`, partitioned by day.
Filtering on the key prefix reads 13 granules (~106K rows) out of 8,386; the
same predicate written as `lower(toString(vin)) = …` defeats the sparse index
and reads every granule of the day's partitions. The API always filters on
`tenant_id` (also enforced by a row policy) and `vin`, and down-samples in SQL
(`toStartOfInterval`) so a chart never transfers more than ~180 points.
