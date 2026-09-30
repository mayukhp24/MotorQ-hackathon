-- Platform-operator read model: cross-tenant aggregates only (no row data),
-- exposed through a SECURITY DEFINER function so the API role never needs
-- BYPASSRLS.
CREATE OR REPLACE FUNCTION platform_tenant_overview()
RETURNS TABLE (tenant_id uuid, slug text, name text, plan_code text, subscription_status text,
               vehicle_quota integer, vehicles bigint, open_alerts bigint, users bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT t.tenant_id, t.slug, t.name, s.plan_code, s.status, s.vehicle_quota,
           (SELECT count(*) FROM vehicle v WHERE v.tenant_id = t.tenant_id),
           (SELECT count(*) FROM alert a WHERE a.tenant_id = t.tenant_id AND a.status = 'OPEN'),
           (SELECT count(*) FROM app_user u WHERE u.tenant_id = t.tenant_id)
    FROM tenant t
    LEFT JOIN subscription s ON s.tenant_id = t.tenant_id AND s.status IN ('TRIAL', 'ACTIVE', 'PAST_DUE')
    ORDER BY t.name
$$;
REVOKE ALL ON FUNCTION platform_tenant_overview() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_tenant_overview() TO fleetpulse_api;
