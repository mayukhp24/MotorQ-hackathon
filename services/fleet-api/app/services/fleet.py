"""Fleet overview and vehicle queries."""

from __future__ import annotations

from typing import Any

from app.context import AppContext
from app.domain.pagination import decode_cursor, encode_cursor
from app.infra import db
from app.security import rbac
from app.security.jwt import Principal
from app.security.masking import mask_location, pseudonym
from app.services import live as live_svc
from app.services.sqlbuild import Where


async def fleet_summary(ctx: AppContext, p: Principal) -> dict[str, Any]:
    tenant = p.tenant_id or ""

    async def load() -> dict[str, Any]:
        async with db.tenant_tx(tenant) as conn:
            vehicles = await conn.fetchval("SELECT count(*) FROM vehicle WHERE tenant_id = $1::uuid", tenant)
            alerts = await conn.fetch(
                "SELECT severity, count(*) AS n FROM alert WHERE tenant_id = $1 AND status = 'OPEN' GROUP BY severity",
                tenant)
            risk = await conn.fetchrow(
                """SELECT count(*) FILTER (WHERE risk_7d >= 0.5) AS high_risk,
                          count(*) FILTER (WHERE risk_7d >= 0.25 AND risk_7d < 0.5) AS medium_risk,
                          coalesce(sum(risk_7d), 0) AS expected_breakdowns_7d,
                          coalesce(sum(expected_savings_usd) FILTER (WHERE risk_7d >= 0.25), 0) AS savings_opportunity,
                          max(scored_on) AS scored_on
                   FROM vehicle_risk_current""")
            wo = await conn.fetchval(
                "SELECT count(*) FROM work_order WHERE status IN ('OPEN', 'SCHEDULED', 'IN_PROGRESS')")
            fleets = await conn.fetch(
                """SELECT f.fleet_id, f.name, d.city, coalesce(c.n, 0) AS vehicles
                   FROM fleet f JOIN depot d ON d.depot_id = f.depot_id
                   LEFT JOIN (SELECT fleet_id, count(*) AS n FROM vehicle WHERE tenant_id = $1::uuid GROUP BY fleet_id) c
                          ON c.fleet_id = f.fleet_id
                   ORDER BY f.name""", tenant)
        return {
            "vehicles": vehicles,
            "open_alerts": {r["severity"]: r["n"] for r in alerts},
            "risk": db.row(risk),
            "open_work_orders": wo,
            "fleets": db.rows(fleets),
        }

    summary = await ctx.cache.get_or_set(tenant, "summary", 30, load)
    summary["live"] = await live_svc.merged_kpi(ctx.live, tenant, ctx.settings.snapshot_stale_s)
    summary["live"]["offline"] = max(0, summary["vehicles"] - summary["live"]["online"])
    return summary


# vehicle_risk_current is a security_barrier view, so Postgres will not push a
# join condition into it: a plain LEFT JOIN scans and sorts every risk row of the
# tenant (55K rows, ~390 ms) to return one page. Unfiltered pages therefore
# probe the view per row through a fenced LATERAL (OFFSET 0 stops the planner
# flattening it back into a join): 51 unique-index lookups, ~2 ms. A risk_min
# filter is selective on the (tenant_id, risk_7d) index, so it keeps the join.
VEHICLE_LIST_SQL = """
SELECT v.vin, v.plate, v.model_year, v.status, v.fleet_id, f.name AS fleet_name, m.name AS model,
       m.oem_code, m.powertrain, r.risk_7d, r.top_component
FROM vehicle v
JOIN fleet f ON f.fleet_id = v.fleet_id
JOIN vehicle_model m ON m.model_id = v.model_id
{risk_join}
{where}
ORDER BY v.vin
LIMIT {limit}"""
RISK_PROBE = ("LEFT JOIN LATERAL (SELECT rc.risk_7d, rc.top_component FROM vehicle_risk_current rc "
              "WHERE rc.vin = v.vin OFFSET 0) r ON true")
RISK_FILTER_JOIN = "JOIN vehicle_risk_current r ON r.vin = v.vin"


async def list_vehicles(ctx: AppContext, p: Principal, *, cursor: str | None, limit: int, fleet_id: str | None,
                        powertrain: str | None, q: str | None, risk_min: float | None) -> dict[str, Any]:
    secret = ctx.settings.cursor_secret
    c = decode_cursor(cursor, secret)
    w = Where().add("v.tenant_id = {}::uuid", p.tenant_id)
    w.add_if(c is not None, "v.vin > {}", (c or {}).get("vin"))
    w.add_if(bool(fleet_id), "v.fleet_id = {}::uuid", fleet_id)
    w.add_if(bool(powertrain), "m.powertrain = {}", powertrain)
    if q:
        term = q.strip().upper()
        w.add("(v.vin LIKE {} OR upper(v.plate) LIKE {})", term + "%", term + "%")
    w.add_if(risk_min is not None, "r.risk_7d >= {}", risk_min)
    sql = VEHICLE_LIST_SQL.format(risk_join=RISK_FILTER_JOIN if risk_min is not None else RISK_PROBE,
                                  where=w.sql(), limit=limit + 1)
    async with db.tenant_tx(p.tenant_id) as conn:
        recs = db.rows(await conn.fetch(sql, *w.args))
    more = len(recs) > limit
    recs = recs[:limit]
    states = await live_svc.live_states(ctx.live, [r["vin"] for r in recs], p.can(rbac.LOCATION_PRECISE))
    for r in recs:
        r["live"] = states.get(r["vin"])
    nxt = encode_cursor({"vin": recs[-1]["vin"]}, secret) if more and recs else None
    return {"items": recs, "next_cursor": nxt}


async def vehicle_detail(ctx: AppContext, p: Principal, vin: str) -> dict[str, Any] | None:
    precise = p.can(rbac.LOCATION_PRECISE)
    async with db.tenant_tx(p.tenant_id) as conn:
        v = await conn.fetchrow(
            """SELECT v.vin, v.plate, v.model_year, v.status, v.in_service_on, v.fleet_id, f.name AS fleet_name,
                      d.name AS depot_name, d.city, d.lat AS depot_lat, d.lon AS depot_lon,
                      m.name AS model, m.oem_code, o.name AS oem_name, m.powertrain, m.body_type,
                      m.battery_kwh, m.tank_l, a.driver_id, dr.full_name AS driver_name, dr.status AS driver_status
               FROM vehicle v
               JOIN fleet f ON f.fleet_id = v.fleet_id
               JOIN depot d ON d.depot_id = f.depot_id
               JOIN vehicle_model m ON m.model_id = v.model_id
               JOIN oem o ON o.oem_code = m.oem_code
               LEFT JOIN vehicle_assignment a ON a.vin = v.vin AND a.valid_to IS NULL
               LEFT JOIN driver dr ON dr.driver_id = a.driver_id
               WHERE v.vin = $1""", vin)
        if v is None:
            return None
        risk = await conn.fetchrow("SELECT * FROM vehicle_risk_current WHERE vin = $1", vin)
        alerts = await conn.fetch(
            """SELECT alert_id, alert_type, severity, title, status, ts, details FROM alert
               WHERE vin = $1 ORDER BY ts DESC LIMIT 10""", vin)
        service = await conn.fetch(
            """SELECT event_type, component, occurred_at, cost_usd, notes FROM service_event
               WHERE vin = $1 ORDER BY occurred_at DESC LIMIT 10""", vin)
        orders = await conn.fetch(
            """SELECT work_order_id, source, component, priority, status, due_on, est_cost_usd, created_at
               FROM work_order WHERE vin = $1 ORDER BY created_at DESC LIMIT 10""", vin)
    out = db.row(v)
    driver_id = str(out.pop("driver_id")) if out.get("driver_id") else None
    name = out.pop("driver_name")
    out["driver"] = None if not driver_id else {
        "driver_id": driver_id, "status": out.pop("driver_status"),
        "name": name if p.can(rbac.DRIVER_PII) else pseudonym(driver_id, ctx.settings.cursor_secret),
    }
    out.pop("driver_status", None)
    out["depot_lat"], out["depot_lon"] = mask_location(out["depot_lat"], out["depot_lon"], precise)
    out["risk"] = db.row(risk)
    out["recent_alerts"] = db.rows(alerts)
    out["service_history"] = db.rows(service)
    out["work_orders"] = db.rows(orders)
    out["live"] = (await live_svc.live_states(ctx.live, [vin], precise)).get(vin)
    return out


async def vehicle_telemetry(ctx: AppContext, p: Principal, vin: str, minutes: int) -> list[dict[str, Any]]:
    bucket = max(10, minutes * 60 // 180)  # ~180 points per chart
    sql = """
SELECT toUnixTimestamp(toStartOfInterval(ts, toIntervalSecond({bucket:UInt32}))) * 1000 AS t,
       round(avg(speed_kmh), 1) AS speed_kmh, round(max(coolant_c), 1) AS coolant_c,
       round(min(batt_v), 2) AS batt_v, round(avg(soc_pct), 1) AS soc_pct, round(avg(fuel_pct), 1) AS fuel_pct,
       round(max(pack_temp_c), 1) AS pack_temp_c, round(avg(rpm)) AS rpm, sum(length(dtc)) AS dtc
FROM fleet.telemetry
WHERE tenant_id = {tenant:String} AND vin = {vin:FixedString(17)} AND ts >= now64(3) - toIntervalMinute({minutes:UInt32})
GROUP BY t ORDER BY t"""
    return await ctx.ch.query(sql, p.tenant_id or "", {"bucket": bucket, "tenant": p.tenant_id, "vin": vin,
                                                      "minutes": minutes})


async def vehicle_daily(ctx: AppContext, p: Principal, vin: str, days: int) -> list[dict[str, Any]]:
    sql = """
SELECT toString(day) AS day, round(max(odo_max) - min(odo_min), 1) AS km,
       round(sum(ign_samples) / 3600, 2) AS engine_h, round(sum(idle_samples) / 3600, 2) AS idle_h,
       max(max_coolant) AS max_coolant_c, round(sum(sum_coolant) / nullIf(sum(n_coolant), 0), 1) AS avg_coolant_c,
       min(min_batt_v) AS min_batt_v, max(max_pack_temp) AS max_pack_temp_c, min(min_soc) AS min_soc,
       sum(harsh_events) AS harsh_events, sum(dtc_total) AS dtc_total, sum(dtc_cooling) AS dtc_cooling,
       sum(dtc_battery) AS dtc_battery, sum(dtc_misfire) AS dtc_misfire, sum(dtc_evpack) AS dtc_evpack
FROM fleet.vehicle_daily
WHERE tenant_id = {tenant:String} AND vin = {vin:FixedString(17)} AND day >= today() - {days:UInt32}
GROUP BY day ORDER BY day"""
    return await ctx.ch.query(sql, p.tenant_id or "", {"tenant": p.tenant_id, "vin": vin, "days": days})
