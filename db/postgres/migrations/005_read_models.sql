-- Precomputed read models for heavy aggregates (CQRS read side).
-- Refreshed by the analytics job (hourly) instead of aggregating millions of
-- trips/alerts on every API request. See docs/sql-optimization.md.

-- Driver safety per rolling window (7 and 30 days).
CREATE MATERIALIZED VIEW mv_driver_safety AS
SELECT t.tenant_id, w.days, t.driver_id,
       count(*)::int                       AS trips,
       round(sum(t.distance_km), 0)        AS km,
       sum(t.harsh_brakes)::int            AS harsh_brakes,
       sum(t.harsh_accels)::int            AS harsh_accels,
       sum(t.overspeed_s)::int             AS overspeed_s,
       sum(t.idle_s)::int                  AS idle_s,
       round(greatest(0, 100
             - sum(t.harsh_brakes + t.harsh_accels) * 100.0 / sum(t.distance_km) * 6
             - sum(t.overspeed_s) / 60.0 * 100.0 / sum(t.distance_km) * 1.5), 1) AS score,
       now()                               AS refreshed_at
FROM trip t
CROSS JOIN (VALUES (7), (30)) AS w(days)
WHERE t.start_ts >= now() - make_interval(days => w.days) AND t.driver_id IS NOT NULL
GROUP BY t.tenant_id, w.days, t.driver_id
HAVING sum(t.distance_km) >= 50;
CREATE UNIQUE INDEX mv_driver_safety_pk ON mv_driver_safety (tenant_id, days, driver_id);
CREATE INDEX mv_driver_safety_score ON mv_driver_safety (tenant_id, days, score);

CREATE VIEW driver_safety WITH (security_barrier) AS
SELECT * FROM mv_driver_safety WHERE tenant_id = app_tenant();

-- Alert counts per day and severity for completed days.
CREATE MATERIALIZED VIEW mv_alert_daily AS
SELECT tenant_id, (ts AT TIME ZONE 'UTC')::date AS day, severity, count(*)::int AS n
FROM alert
WHERE ts < date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
GROUP BY 1, 2, 3;
CREATE UNIQUE INDEX mv_alert_daily_pk ON mv_alert_daily (tenant_id, day, severity);

CREATE VIEW alert_daily WITH (security_barrier) AS
SELECT * FROM mv_alert_daily WHERE tenant_id = app_tenant();

CREATE OR REPLACE FUNCTION refresh_read_models() RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY mv_driver_safety;
    REFRESH MATERIALIZED VIEW CONCURRENTLY mv_alert_daily;
END $$;

GRANT SELECT ON driver_safety, alert_daily TO fleetpulse_api;
GRANT EXECUTE ON FUNCTION refresh_read_models() TO fleetpulse_writer, fleetpulse_api;
