-- FleetPulse analytical store (ClickHouse 24.x).
-- Telemetry is consumed straight from Kafka (no custom sink code), stored
-- column-compressed, partitioned by day and ordered by (tenant, vin, ts) so
-- per-vehicle history is a narrow range scan and fleet-wide scans prune by
-- day. ${VARS} are substituted by the migrator from the environment.

CREATE DATABASE IF NOT EXISTS fleet;

-- ------------------------------------------------------------- telemetry
CREATE TABLE IF NOT EXISTS fleet.telemetry
(
    tenant_id       LowCardinality(String),
    vin             FixedString(17),
    ts              DateTime64(3, 'UTC') CODEC(DoubleDelta, ZSTD(1)),
    seq             Int64 CODEC(Delta, ZSTD(1)),
    oem             LowCardinality(String),
    fleet_id        LowCardinality(String),
    powertrain      LowCardinality(String),
    lat             Float64 CODEC(Gorilla, ZSTD(1)),
    lon             Float64 CODEC(Gorilla, ZSTD(1)),
    geohash         String CODEC(ZSTD(1)),
    heading_deg     Float32 CODEC(ZSTD(1)),
    speed_kmh       Float32 CODEC(Gorilla, ZSTD(1)),
    accel_mps2      Float32 CODEC(ZSTD(1)),
    odo_km          Float64 CODEC(Gorilla, ZSTD(1)),
    ignition        Bool,
    charging        Bool,
    rpm             Nullable(Float32),
    coolant_c       Nullable(Float32),
    engine_load_pct Nullable(Float32),
    fuel_pct        Nullable(Float32),
    soc_pct         Nullable(Float32),
    pack_temp_c     Nullable(Float32),
    batt_v          Nullable(Float32),
    tyre_min_kpa    Nullable(Float32),
    dtc             Array(LowCardinality(String)),
    evt             Array(LowCardinality(String)),
    ingest_ts       DateTime64(3, 'UTC'),
    proc_ts         DateTime64(3, 'UTC'),
    INDEX idx_dtc dtc TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = ReplacingMergeTree(proc_ts)          -- at-least-once duplicates collapse on merge
PARTITION BY toYYYYMMDD(ts)
ORDER BY (tenant_id, vin, ts, seq)
TTL toDateTime(ts) + INTERVAL ${CH_HOT_DAYS} DAY TO VOLUME 'cold',
    toDateTime(ts) + INTERVAL ${CH_RETENTION_DAYS} DAY DELETE
SETTINGS storage_policy = 'hot_cold';

CREATE TABLE IF NOT EXISTS fleet.telemetry_queue
(
    event_id String, v UInt8, vin String, oem String, ts_ms Int64, seq Int64,
    lat Float64, lon Float64, heading_deg Float32, speed_kmh Float32, accel_mps2 Float32, odo_km Float64,
    ignition Bool, charging Bool,
    rpm Nullable(Float32), coolant_c Nullable(Float32), engine_load_pct Nullable(Float32),
    fuel_pct Nullable(Float32), soc_pct Nullable(Float32), pack_temp_c Nullable(Float32),
    batt_v Nullable(Float32), tyre_min_kpa Nullable(Float32),
    dtc Array(String), evt Array(String), ingest_ms Int64,
    tenant_id String, fleet_id String, geohash String, powertrain String, proc_ms Int64
)
ENGINE = Kafka
SETTINGS kafka_broker_list = '${KAFKA_BROKERS}',
         kafka_topic_list = 'telemetry.enriched.v1',
         kafka_group_name = 'clickhouse-telemetry',
         kafka_format = 'JSONEachRow',
         kafka_num_consumers = ${CH_KAFKA_CONSUMERS},
         kafka_max_block_size = 262144,
         kafka_flush_interval_ms = 1000,
         kafka_skip_broken_messages = 1000,
         input_format_skip_unknown_fields = 1;   -- forward-compatible schema evolution

CREATE MATERIALIZED VIEW IF NOT EXISTS fleet.telemetry_mv TO fleet.telemetry AS
SELECT tenant_id, toFixedString(vin, 17) AS vin, fromUnixTimestamp64Milli(ts_ms, 'UTC') AS ts, seq, oem,
       fleet_id, powertrain, lat, lon, geohash, heading_deg, speed_kmh, accel_mps2, odo_km, ignition,
       charging, rpm, coolant_c, engine_load_pct, fuel_pct, soc_pct, pack_temp_c, batt_v, tyre_min_kpa,
       dtc, evt, fromUnixTimestamp64Milli(ingest_ms, 'UTC') AS ingest_ts,
       fromUnixTimestamp64Milli(proc_ms, 'UTC') AS proc_ts
FROM fleet.telemetry_queue;

-- ---------------------------------------------- daily vehicle aggregates
-- Feature store for predictive maintenance. SimpleAggregateFunction columns
-- merge incrementally, and the historical back-fill can insert plain values.
CREATE TABLE IF NOT EXISTS fleet.vehicle_daily
(
    tenant_id         LowCardinality(String),
    vin               FixedString(17),
    day               Date,
    samples           SimpleAggregateFunction(sum, UInt64),
    ign_samples       SimpleAggregateFunction(sum, UInt64),
    idle_samples      SimpleAggregateFunction(sum, UInt64),
    moving_samples    SimpleAggregateFunction(sum, UInt64),
    odo_min           SimpleAggregateFunction(min, Float64),
    odo_max           SimpleAggregateFunction(max, Float64),
    max_speed         SimpleAggregateFunction(max, Float32),
    max_coolant       SimpleAggregateFunction(max, Nullable(Float32)),
    sum_coolant       SimpleAggregateFunction(sum, Float64),
    n_coolant         SimpleAggregateFunction(sum, UInt64),
    min_batt_v        SimpleAggregateFunction(min, Nullable(Float32)),
    sum_batt_v        SimpleAggregateFunction(sum, Float64),
    n_batt_v          SimpleAggregateFunction(sum, UInt64),
    max_pack_temp     SimpleAggregateFunction(max, Nullable(Float32)),
    min_soc           SimpleAggregateFunction(min, Nullable(Float32)),
    harsh_events      SimpleAggregateFunction(sum, UInt64),
    overspeed_samples SimpleAggregateFunction(sum, UInt64),
    dtc_total         SimpleAggregateFunction(sum, UInt64),
    dtc_cooling       SimpleAggregateFunction(sum, UInt64),
    dtc_battery       SimpleAggregateFunction(sum, UInt64),
    dtc_misfire       SimpleAggregateFunction(sum, UInt64),
    dtc_evpack        SimpleAggregateFunction(sum, UInt64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (tenant_id, vin, day)
TTL day + INTERVAL 730 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS fleet.vehicle_daily_mv TO fleet.vehicle_daily AS
SELECT tenant_id, vin, toDate(ts) AS day,
       count() AS samples,
       countIf(ignition) AS ign_samples,
       countIf(ignition AND speed_kmh < 1 AND NOT charging) AS idle_samples,
       countIf(speed_kmh >= 3) AS moving_samples,
       min(odo_km) AS odo_min, max(odo_km) AS odo_max, max(speed_kmh) AS max_speed,
       maxIf(coolant_c, ignition) AS max_coolant,
       toFloat64(ifNull(sumIf(coolant_c, ignition), 0)) AS sum_coolant,
       countIf(ignition AND coolant_c IS NOT NULL) AS n_coolant,
       minIf(batt_v, ignition) AS min_batt_v,
       toFloat64(ifNull(sumIf(batt_v, ignition), 0)) AS sum_batt_v,
       countIf(ignition AND batt_v IS NOT NULL) AS n_batt_v,
       max(pack_temp_c) AS max_pack_temp, min(soc_pct) AS min_soc,
       countIf(has(evt, 'HARSH_BRAKE') OR has(evt, 'HARSH_ACCEL') OR accel_mps2 <= -6.5 OR accel_mps2 >= 4.5) AS harsh_events,
       countIf(speed_kmh > 120) AS overspeed_samples,
       sum(length(dtc)) AS dtc_total,
       sum(arrayCount(x -> x IN ('P0115', 'P0116', 'P0128', 'P0217', 'P0480'), dtc)) AS dtc_cooling,
       sum(arrayCount(x -> x IN ('P0562', 'P0563', 'P0620', 'P0A0F'), dtc)) AS dtc_battery,
       sum(arrayCount(x -> x IN ('P0300', 'P0301', 'P0302', 'P0303', 'P0304'), dtc)) AS dtc_misfire,
       sum(arrayCount(x -> x IN ('P0A7F', 'P0AFA', 'P0A80', 'P0AA6', 'P0C73'), dtc)) AS dtc_evpack
FROM fleet.telemetry
GROUP BY tenant_id, vin, day;

-- ----------------------------------------------------- hourly fleet rollup
CREATE TABLE IF NOT EXISTS fleet.fleet_hourly
(
    tenant_id      LowCardinality(String),
    fleet_id       LowCardinality(String),
    hour           DateTime('UTC'),
    events         SimpleAggregateFunction(sum, UInt64),
    vehicles       AggregateFunction(uniq, FixedString(17)),
    moving_samples SimpleAggregateFunction(sum, UInt64),
    speed_sum      SimpleAggregateFunction(sum, Float64),
    idle_samples   SimpleAggregateFunction(sum, UInt64),
    harsh          SimpleAggregateFunction(sum, UInt64),
    dtc            SimpleAggregateFunction(sum, UInt64),
    latency_ms_sum SimpleAggregateFunction(sum, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (tenant_id, fleet_id, hour)
TTL hour + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS fleet.fleet_hourly_mv TO fleet.fleet_hourly AS
SELECT tenant_id, fleet_id, toStartOfHour(ts) AS hour,
       count() AS events, uniqState(vin) AS vehicles,
       countIf(speed_kmh >= 3) AS moving_samples, sumIf(toFloat64(speed_kmh), speed_kmh >= 3) AS speed_sum,
       countIf(ignition AND speed_kmh < 1 AND NOT charging) AS idle_samples,
       countIf(has(evt, 'HARSH_BRAKE') OR has(evt, 'HARSH_ACCEL')) AS harsh,
       sum(length(dtc)) AS dtc,
       sum(toFloat64(dateDiff('millisecond', ingest_ts, proc_ts))) AS latency_ms_sum
FROM fleet.telemetry
GROUP BY tenant_id, fleet_id, hour;

-- --------------------------------------------------------- alert history
CREATE TABLE IF NOT EXISTS fleet.alerts_history
(
    tenant_id   LowCardinality(String),
    fleet_id    LowCardinality(String),
    vin         FixedString(17),
    alert_id    UUID,
    type        LowCardinality(String),
    severity    LowCardinality(String),
    ts          DateTime64(3, 'UTC'),
    detected_ts DateTime64(3, 'UTC'),
    latency_ms  Int64
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (tenant_id, ts, alert_id);

CREATE TABLE IF NOT EXISTS fleet.alerts_queue
(
    alert_id String, tenant_id String, fleet_id String, vin String, type String, severity String,
    ts_ms Int64, ingest_ms Int64, detected_ms Int64
)
ENGINE = Kafka
SETTINGS kafka_broker_list = '${KAFKA_BROKERS}', kafka_topic_list = 'alerts.v1',
         kafka_group_name = 'clickhouse-alerts', kafka_format = 'JSONEachRow',
         kafka_skip_broken_messages = 100, input_format_skip_unknown_fields = 1;

CREATE MATERIALIZED VIEW IF NOT EXISTS fleet.alerts_mv TO fleet.alerts_history AS
SELECT tenant_id, fleet_id, toFixedString(vin, 17) AS vin, toUUID(alert_id) AS alert_id, type, severity,
       fromUnixTimestamp64Milli(ts_ms, 'UTC') AS ts, fromUnixTimestamp64Milli(detected_ms, 'UTC') AS detected_ts,
       detected_ms - ts_ms AS latency_ms
FROM fleet.alerts_queue;

-- ------------------------------------------------ data-quality (DLQ) view
CREATE TABLE IF NOT EXISTS fleet.ingest_rejects
(
    ts     DateTime64(3, 'UTC'),
    oem    LowCardinality(String),
    reason String,
    raw    String CODEC(ZSTD(3))
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (oem, ts)
TTL toDateTime(ts) + INTERVAL 14 DAY;

CREATE TABLE IF NOT EXISTS fleet.dlq_queue (oem String, reason String, raw String)
ENGINE = Kafka
SETTINGS kafka_broker_list = '${KAFKA_BROKERS}', kafka_topic_list = 'telemetry.dlq.v1',
         kafka_group_name = 'clickhouse-dlq', kafka_format = 'JSONEachRow',
         kafka_skip_broken_messages = 100000, input_format_skip_unknown_fields = 1;

CREATE MATERIALIZED VIEW IF NOT EXISTS fleet.dlq_mv TO fleet.ingest_rejects AS
SELECT ifNull(_timestamp_ms, now64(3)) AS ts, oem, reason, raw FROM fleet.dlq_queue;

-- ------------------------------------------ read-only API user + row policy
-- Tenant isolation is enforced by the database, not just the API: the API
-- must set SQL_tenant_id on every query or it sees nothing.
CREATE USER IF NOT EXISTS fleetpulse_ro IDENTIFIED WITH sha256_password BY '${CH_RO_PASSWORD}'
    SETTINGS readonly = 2, max_execution_time = 20, max_memory_usage = 4000000000;
GRANT SELECT ON fleet.* TO fleetpulse_ro;
CREATE ROW POLICY IF NOT EXISTS tenant_isolation ON fleet.telemetry, fleet.vehicle_daily, fleet.fleet_hourly, fleet.alerts_history
    FOR SELECT USING tenant_id = getSetting('SQL_tenant_id') TO fleetpulse_ro;
