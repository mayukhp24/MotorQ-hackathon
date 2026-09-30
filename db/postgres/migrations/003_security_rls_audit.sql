-- Tenant isolation with row-level security, least-privilege roles and a
-- tamper-evident (hash-chained), append-only audit log.

-- The API sets `SET LOCAL app.tenant_id = '<uuid>'` in every transaction.
CREATE OR REPLACE FUNCTION app_tenant() RETURNS uuid
LANGUAGE sql STABLE AS $$ SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid $$;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tenant', 'subscription', 'app_user', 'depot', 'fleet', 'vehicle', 'driver',
                             'vehicle_assignment', 'trip', 'alert', 'work_order', 'service_event',
                             'risk_score', 'erasure_request', 'agent_action']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant_id = app_tenant()) WITH CHECK (tenant_id = app_tenant())', t);
    END LOOP;
END $$;

-- Audit: tenants read their own trail; platform events (e.g. failed logins
-- before a tenant is known) are written with tenant_id NULL.
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_read ON audit_log FOR SELECT USING (tenant_id = app_tenant());
CREATE POLICY audit_insert ON audit_log FOR INSERT WITH CHECK (tenant_id IS NULL OR tenant_id = app_tenant());

-- Tenant-scoped view over the risk materialised view (MVs cannot carry RLS).
CREATE VIEW vehicle_risk_current WITH (security_barrier) AS
SELECT * FROM mv_vehicle_risk_current WHERE tenant_id = app_tenant();

-- Login happens before a tenant is known: a narrow SECURITY DEFINER lookup.
CREATE OR REPLACE FUNCTION auth_lookup(p_email text)
RETURNS TABLE (user_id uuid, tenant_id uuid, full_name text, password_hash text, status text, roles text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT u.user_id, u.tenant_id, u.full_name, u.password_hash, u.status,
           COALESCE(array_agg(r.role_code ORDER BY r.role_code) FILTER (WHERE r.role_code IS NOT NULL), '{}')
    FROM app_user u LEFT JOIN user_role r ON r.user_id = u.user_id
    WHERE u.email = lower(p_email)
    GROUP BY u.user_id
$$;

-- ------------------------------------------------------------- audit chain
-- Each row's hash covers its content and the previous row's hash, so any
-- edit or deletion breaks the chain (verified by audit_verify()).
CREATE OR REPLACE FUNCTION audit_row_digest(prev bytea, r audit_log) RETURNS bytea
LANGUAGE sql IMMUTABLE AS $$
    SELECT digest(COALESCE(prev, '\x'::bytea) || convert_to(concat_ws('|',
        r.audit_id::text, to_char(r.ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'),
        COALESCE(r.tenant_id::text, ''), COALESCE(r.actor_id::text, ''), r.actor_type, r.action,
        r.resource_type, COALESCE(r.resource_id, ''), r.outcome, COALESCE(host(r.ip), ''),
        COALESCE(r.request_id, ''), r.details::text), 'UTF8'), 'sha256')
$$;

CREATE OR REPLACE FUNCTION audit_chain() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE last bytea;
BEGIN
    PERFORM pg_advisory_xact_lock(727274);           -- serialise the chain
    NEW.audit_id := nextval('audit_log_audit_id_seq'); -- id order == chain order
    NEW.ts := clock_timestamp();
    SELECT hash INTO last FROM audit_log ORDER BY audit_id DESC LIMIT 1;
    NEW.prev_hash := last;
    NEW.hash := audit_row_digest(last, NEW);
    RETURN NEW;
END $$;
CREATE TRIGGER audit_chain BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_chain();

CREATE OR REPLACE FUNCTION audit_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END $$;
CREATE TRIGGER audit_no_update BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_immutable();

-- Returns the first audit_id whose hash does not verify (NULL = intact).
CREATE OR REPLACE FUNCTION audit_verify() RETURNS bigint
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public AS $$
DECLARE r audit_log; prev bytea := NULL;
BEGIN
    FOR r IN SELECT * FROM audit_log ORDER BY audit_id LOOP
        IF r.prev_hash IS DISTINCT FROM prev OR r.hash <> audit_row_digest(prev, r) THEN
            RETURN r.audit_id;
        END IF;
        prev := r.hash;
    END LOOP;
    RETURN NULL;
END $$;

-- Only the owner may refresh a materialised view; expose a narrow wrapper.
CREATE OR REPLACE FUNCTION refresh_vehicle_risk() RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY mv_vehicle_risk_current;
END $$;

-- ------------------------------------------------------------------ roles
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fleetpulse_api') THEN
        CREATE ROLE fleetpulse_api LOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fleetpulse_writer') THEN
        CREATE ROLE fleetpulse_writer LOGIN BYPASSRLS;
    END IF;
END $$;

-- API: tenant-scoped reads, a few narrowly scoped writes. No DDL, no BYPASSRLS.
GRANT USAGE ON SCHEMA public TO fleetpulse_api, fleetpulse_writer;
GRANT SELECT ON plan, role, oem, vehicle_model, dtc_code, fault_knowledge, model_version TO fleetpulse_api;
GRANT SELECT ON tenant, subscription, app_user, user_role, depot, fleet, vehicle, driver, vehicle_assignment,
                trip, alert, work_order, service_event, risk_score, audit_log, erasure_request, agent_action,
                vehicle_risk_current TO fleetpulse_api;
GRANT UPDATE (status, acknowledged_by, acknowledged_at, resolved_at) ON alert TO fleetpulse_api;
GRANT INSERT, UPDATE ON work_order, erasure_request, agent_action TO fleetpulse_api;
GRANT UPDATE (full_name, license_hash, license_no_enc, phone_enc, status, erased_at, safety_opt_in) ON driver TO fleetpulse_api;
GRANT DELETE ON vehicle_assignment TO fleetpulse_api;
GRANT UPDATE (driver_id, start_geohash, end_geohash) ON trip TO fleetpulse_api;
GRANT UPDATE (last_login_at) ON app_user TO fleetpulse_api;
GRANT INSERT ON audit_log TO fleetpulse_api;
GRANT USAGE ON SEQUENCE audit_log_audit_id_seq TO fleetpulse_api, fleetpulse_writer;
GRANT EXECUTE ON FUNCTION auth_lookup(text), audit_verify(), app_tenant() TO fleetpulse_api;

-- Writer (stream sinks, analytics jobs, seeding): bypasses RLS by design,
-- never exposed to end users.
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA public TO fleetpulse_writer;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO fleetpulse_writer;
GRANT DELETE ON risk_score, fault_knowledge, trip, alert, service_event, vehicle_assignment TO fleetpulse_writer;
GRANT EXECUTE ON FUNCTION refresh_vehicle_risk(), ensure_trip_partitions(date, integer) TO fleetpulse_writer;
REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM PUBLIC, fleetpulse_api, fleetpulse_writer;
