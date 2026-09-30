#!/usr/bin/env bash
# Captures EXPLAIN (ANALYZE, BUFFERS) before/after pairs for the query
# optimisations described in docs/sql-optimization.md, against the running
# docker compose stack. Queries run as the least-privilege API role with the
# tenant GUC set, so RLS and security-barrier views are in play exactly as in
# production. "Before" variants that need a missing index drop it inside a
# transaction that is rolled back.
#
#   scripts/explain-evidence.sh            # writes docs/evidence/explain/*.txt
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=docs/evidence/explain
mkdir -p "$OUT"
PG="docker compose exec -T postgres psql -U postgres -d fleetpulse -v ON_ERROR_STOP=1 -X -q"
TENANT=$($PG -At -c "SELECT tenant_id FROM tenant WHERE slug = 'acme-logistics'")
CURSOR_VIN=$($PG -At -c "SELECT vin FROM vehicle WHERE tenant_id = '$TENANT' ORDER BY vin OFFSET 50000 LIMIT 1")

# run NAME LABEL SETUP_SQL QUERY_SQL
run() {
  local name=$1 label=$2 setup=$3 query=$4
  $PG <<SQL >> "$OUT/$name.txt"
\echo '==================== $label ===================='
BEGIN;
$setup
SET LOCAL ROLE fleetpulse_api;
SELECT set_config('app.tenant_id', '$TENANT', true) \g /dev/null
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) $query;
ROLLBACK;
SQL
}

summary() {
  local name=$1
  local times
  times=$(grep -o 'Execution Time: [0-9.]*' "$OUT/$name.txt" | awk '{print $3}' | paste -sd' ')
  printf '%-28s before/after ms: %s\n' "$name" "$times"
}

pair() { # NAME TITLE BEFORE_SETUP BEFORE_QUERY AFTER_SETUP AFTER_QUERY
  : > "$OUT/$1.txt"
  echo "# $2" >> "$OUT/$1.txt"
  run "$1" BEFORE "$3" "$4"
  run "$1" AFTER "$5" "$6"
  summary "$1"
}

VEH_COLS="v.vin, v.plate, v.model_year, v.status, f.name AS fleet_name, m.name AS model, m.powertrain, r.risk_7d, r.top_component"
VEH_FROM="vehicle v JOIN fleet f ON f.fleet_id = v.fleet_id JOIN vehicle_model m ON m.model_id = v.model_id"

pair 01-vehicle-list "Vehicle list page: security-barrier view join vs fenced LATERAL probe" \
  "" "SELECT $VEH_COLS FROM $VEH_FROM LEFT JOIN vehicle_risk_current r ON r.vin = v.vin WHERE v.tenant_id = '$TENANT' ORDER BY v.vin LIMIT 51" \
  "" "SELECT $VEH_COLS FROM $VEH_FROM LEFT JOIN LATERAL (SELECT rc.risk_7d, rc.top_component FROM vehicle_risk_current rc WHERE rc.vin = v.vin OFFSET 0) r ON true WHERE v.tenant_id = '$TENANT' ORDER BY v.vin LIMIT 51"

pair 02-deep-pagination "Page 1,000 of the vehicle list: OFFSET vs keyset (vin > cursor)" \
  "" "SELECT v.vin, v.plate FROM vehicle v WHERE v.tenant_id = '$TENANT' ORDER BY v.vin OFFSET 50000 LIMIT 51" \
  "" "SELECT v.vin, v.plate FROM vehicle v WHERE v.tenant_id = '$TENANT' AND v.vin > '$CURSOR_VIN' ORDER BY v.vin LIMIT 51"

pair 03-open-alerts "Open-alerts feed: without vs with the partial index (tenant_id, ts DESC) WHERE status = 'OPEN'" \
  "DROP INDEX alert_open_tenant_ts;" "SELECT alert_id, vin, alert_type, severity, ts FROM alert WHERE tenant_id = '$TENANT' AND status = 'OPEN' ORDER BY ts DESC, alert_id DESC LIMIT 51" \
  "" "SELECT alert_id, vin, alert_type, severity, ts FROM alert WHERE tenant_id = '$TENANT' AND status = 'OPEN' ORDER BY ts DESC, alert_id DESC LIMIT 51"

pair 04-risk-ranking "Riskiest vehicles: DISTINCT ON over score history vs materialised current-risk read model" \
  "" "SELECT * FROM (SELECT DISTINCT ON (r.vin) r.vin, r.risk_7d, r.top_component FROM risk_score r JOIN model_version mv ON mv.model_version = r.model_version AND mv.is_active WHERE r.tenant_id = '$TENANT' ORDER BY r.vin, r.scored_on DESC) x WHERE risk_7d >= 0.2 ORDER BY risk_7d DESC, vin DESC LIMIT 51" \
  "" "SELECT vin, risk_7d, top_component FROM vehicle_risk_current WHERE risk_7d >= 0.2 ORDER BY risk_7d DESC, vin DESC LIMIT 51"

pair 05-driver-safety "Driver safety ranking (30 days): aggregate over trips vs mv_driver_safety" \
  "" "SELECT t.driver_id, count(*) AS trips, sum(t.distance_km) AS km, greatest(0, 100 - sum(t.harsh_brakes + t.harsh_accels) * 100.0 / sum(t.distance_km) * 6 - sum(t.overspeed_s) / 60.0 * 100.0 / sum(t.distance_km) * 1.5) AS score FROM trip t WHERE t.tenant_id = '$TENANT' AND t.start_ts >= now() - interval '30 days' AND t.driver_id IS NOT NULL GROUP BY t.driver_id HAVING sum(t.distance_km) >= 50 ORDER BY score LIMIT 20" \
  "" "SELECT driver_id, trips, km, score FROM driver_safety WHERE days = 30 ORDER BY score LIMIT 20"

pair 06-alert-trend "Alert trend (14 days by severity): aggregate over alerts vs mv_alert_daily" \
  "" "SELECT (ts AT TIME ZONE 'UTC')::date AS day, severity, count(*) FROM alert WHERE tenant_id = '$TENANT' AND ts >= now() - interval '14 days' AND ts < date_trunc('day', now()) GROUP BY 1, 2 ORDER BY 1" \
  "" "SELECT day, severity, n FROM alert_daily WHERE day >= current_date - 14 ORDER BY day"

# ClickHouse: primary-key pruning on (tenant_id, vin, ts, seq).
CH="docker compose exec -T clickhouse clickhouse-client --password ${CLICKHOUSE_PASSWORD:-clickhouse-local-only}"
VIN=$($PG -At -c "SELECT vin FROM vehicle WHERE tenant_id = '$TENANT' ORDER BY vin LIMIT 1")
{
  echo "# ClickHouse: one vehicle's last 6 h of telemetry - sort-key prefix (tenant_id, vin, ts) vs the same"
  echo "# predicate hidden behind a function on the key column (defeats primary-key pruning)"
  for q in "SELECT toStartOfMinute(ts) m, avg(speed_kmh), max(coolant_c) FROM fleet.telemetry WHERE tenant_id = '$TENANT' AND vin = '$VIN' AND ts >= now() - INTERVAL 6 HOUR GROUP BY m" \
           "SELECT toStartOfMinute(ts) m, avg(speed_kmh), max(coolant_c) FROM fleet.telemetry WHERE lower(toString(vin)) = lower('$VIN') AND ts >= now() - INTERVAL 6 HOUR GROUP BY m"; do
    echo; echo "== $q"
    $CH -q "EXPLAIN indexes = 1 $q" 2>&1 | grep -E "Keys|Condition|Parts|Granules" | head -12 || true
  done
} > "$OUT/07-clickhouse-pk.txt" 2>&1
echo "wrote $OUT/"
