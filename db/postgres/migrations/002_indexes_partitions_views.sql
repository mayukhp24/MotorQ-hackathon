-- Indexes chosen from the query log of the API (see docs/sql-optimization.md
-- for EXPLAIN ANALYZE before/after), trip partitions and read models.

-- Keyset pagination of the vehicle list per tenant.
CREATE INDEX vehicle_tenant_vin ON vehicle (tenant_id, vin);
CREATE INDEX vehicle_fleet ON vehicle (fleet_id);
CREATE INDEX driver_tenant ON driver (tenant_id);
CREATE INDEX fleet_tenant ON fleet (tenant_id);

-- Dashboard "open alerts, newest first": partial index keeps only the small
-- OPEN working set (most alerts are resolved) and matches the sort order.
CREATE INDEX alert_open_tenant_ts ON alert (tenant_id, ts DESC) WHERE status = 'OPEN';
CREATE INDEX alert_tenant_ts ON alert (tenant_id, ts DESC, alert_id);
CREATE INDEX alert_vin_ts ON alert (vin, ts DESC);

-- Trips: covering index lets the driver-safety aggregate run as an
-- index-only scan inside the pruned monthly partitions.
CREATE INDEX trip_tenant_start ON trip (tenant_id, start_ts)
    INCLUDE (driver_id, distance_km, harsh_brakes, harsh_accels, overspeed_s, idle_s);
CREATE INDEX trip_vin_start ON trip (vin, start_ts DESC);

CREATE INDEX work_order_tenant_status ON work_order (tenant_id, status, due_on);
CREATE INDEX work_order_vin ON work_order (vin);
CREATE INDEX service_event_vin_ts ON service_event (vin, occurred_at);
CREATE INDEX service_event_tenant_ts ON service_event (tenant_id, occurred_at);
CREATE INDEX risk_score_tenant_day ON risk_score (tenant_id, scored_on);
CREATE INDEX audit_tenant_ts ON audit_log (tenant_id, ts DESC);
CREATE INDEX audit_actor_ts ON audit_log (actor_id, ts DESC);
CREATE INDEX erasure_tenant ON erasure_request (tenant_id, requested_at DESC);
CREATE INDEX agent_action_tenant ON agent_action (tenant_id, created_at DESC);

-- Approximate nearest-neighbour search over the fault knowledge base.
CREATE INDEX fault_knowledge_embedding ON fault_knowledge USING hnsw (embedding vector_cosine_ops);

-- ------------------------------------------------------ trip partitions
CREATE OR REPLACE FUNCTION ensure_trip_partitions(from_month date, months integer)
RETURNS integer LANGUAGE plpgsql AS $$
DECLARE
    m date;
    created integer := 0;
    part text;
BEGIN
    FOR i IN 0 .. months - 1 LOOP
        m := date_trunc('month', from_month)::date + (i || ' month')::interval;
        part := format('trip_%s', to_char(m, 'YYYY_MM'));
        IF to_regclass(part) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF trip FOR VALUES FROM (%L) TO (%L)',
                           part, m, (m + interval '1 month')::date);
            created := created + 1;
        END IF;
    END LOOP;
    RETURN created;
END $$;

SELECT ensure_trip_partitions(date '2026-01-01', 24);
CREATE TABLE IF NOT EXISTS trip_default PARTITION OF trip DEFAULT;

-- ------------------------------------------------- latest risk read model
-- Materialised view: the API reads the latest score per vehicle without a
-- DISTINCT ON over the full history. Refreshed CONCURRENTLY after scoring.
CREATE MATERIALIZED VIEW mv_vehicle_risk_current AS
SELECT DISTINCT ON (r.vin)
       r.vin, r.tenant_id, r.scored_on, r.model_version, r.risk_7d, r.top_component,
       r.top_factors, r.est_breakdown_cost, r.expected_savings_usd
FROM risk_score r
JOIN model_version mv ON mv.model_version = r.model_version AND mv.is_active
ORDER BY r.vin, r.scored_on DESC;
CREATE UNIQUE INDEX mv_vehicle_risk_current_vin ON mv_vehicle_risk_current (vin);
CREATE INDEX mv_vehicle_risk_current_tenant_risk ON mv_vehicle_risk_current (tenant_id, risk_7d DESC);
