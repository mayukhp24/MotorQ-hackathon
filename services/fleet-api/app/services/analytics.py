"""Batch analytics over historical data (ClickHouse + PostgreSQL)."""

from __future__ import annotations

from typing import Any

from app.context import AppContext
from app.infra import db
from app.security import rbac
from app.security.jwt import Principal
from app.security.masking import pseudonym

# Idle cost assumptions (documented in docs/solution): diesel/petrol idle burn
# ~0.9 L/h at $1.15/L; EV cabin/HVAC load ~2 kW at $0.18/kWh.
ICE_IDLE_USD_PER_H = 0.9 * 1.15
EV_IDLE_USD_PER_H = 2.0 * 0.18


async def fleet_hourly(ctx: AppContext, p: Principal, hours: int) -> list[dict[str, Any]]:
    sql = """
SELECT toUnixTimestamp(hour) * 1000 AS t, sum(events) AS events, uniqMerge(vehicles) AS vehicles,
       round(sum(speed_sum) / nullIf(sum(moving_samples), 0), 1) AS avg_speed_kmh,
       sum(idle_samples) AS idle_samples, sum(harsh) AS harsh_events, sum(dtc) AS dtc,
       round(sum(latency_ms_sum) / nullIf(sum(events), 0), 1) AS avg_pipeline_latency_ms
FROM fleet.fleet_hourly
WHERE tenant_id = {tenant:String} AND hour >= now() - toIntervalHour({hours:UInt32})
GROUP BY hour ORDER BY hour"""

    async def load() -> list[dict[str, Any]]:
        return await ctx.ch.query(sql, p.tenant_id or "", {"tenant": p.tenant_id, "hours": hours})

    return await ctx.cache.get_or_set(p.tenant_id or "", f"hourly:{hours}", 30, load)


async def idle_cost(ctx: AppContext, p: Principal, days: int) -> dict[str, Any]:
    sql = f"""
SELECT vin, round(sum(idle_samples) / 3600, 2) AS idle_h, sum(n_coolant) > 0 AS combustion,
       round(sum(idle_samples) / 3600 * if(sum(n_coolant) > 0, {ICE_IDLE_USD_PER_H}, {EV_IDLE_USD_PER_H}), 2) AS cost_usd
FROM fleet.vehicle_daily
WHERE tenant_id = {{tenant:String}} AND day >= today() - {{days:UInt32}} AND day < today()
GROUP BY vin ORDER BY cost_usd DESC LIMIT 20"""
    total_sql = f"""
SELECT round(sum(idle_h), 0) AS idle_h, round(sum(cost), 0) AS cost_usd, count() AS vehicles
FROM (SELECT vin, sum(idle_samples) / 3600 AS idle_h,
             sum(idle_samples) / 3600 * if(sum(n_coolant) > 0, {ICE_IDLE_USD_PER_H}, {EV_IDLE_USD_PER_H}) AS cost
      FROM fleet.vehicle_daily
      WHERE tenant_id = {{tenant:String}} AND day >= today() - {{days:UInt32}} AND day < today() GROUP BY vin)"""
    daily_sql = f"""
SELECT toString(day) AS day, round(sum(idle_samples) / 3600, 0) AS idle_h,
       round(sum(idle_samples) / 3600 * {ICE_IDLE_USD_PER_H}, 0) AS approx_cost_usd
FROM fleet.vehicle_daily
WHERE tenant_id = {{tenant:String}} AND day >= today() - {{days:UInt32}} AND day < today()
GROUP BY day ORDER BY day"""
    params = {"tenant": p.tenant_id, "days": days}

    async def load() -> dict[str, Any]:
        top = await ctx.ch.query(sql, p.tenant_id or "", params)
        total = await ctx.ch.query(total_sql, p.tenant_id or "", params)
        daily = await ctx.ch.query(daily_sql, p.tenant_id or "", params)
        t = total[0] if total else {"idle_h": 0, "cost_usd": 0, "vehicles": 0}
        # A realistic target: cut idling 30% via policy + driver coaching.
        t["savings_at_30pct_usd"] = round(float(t.get("cost_usd") or 0) * 0.3)
        t["annualised_cost_usd"] = round(float(t.get("cost_usd") or 0) * 365 / max(days, 1))
        return {"days": days, "total": t, "top_vehicles": top, "daily": daily}

    return await ctx.cache.get_or_set(p.tenant_id or "", f"idle:{days}", 120, load)


def safety_score(km: float, harsh: int, overspeed_s: int) -> float:
    """0-100 score: harsh events and overspeed minutes per 100 km (mirrors
    mv_driver_safety in migration 005)."""
    if km <= 0:
        return 100.0
    per100 = 100.0 / km
    penalty = harsh * per100 * 6 + (overspeed_s / 60) * per100 * 1.5
    return round(max(0.0, 100.0 - penalty), 1)


async def driver_safety(ctx: AppContext, p: Principal, days: int, limit: int) -> dict[str, Any]:
    """Reads the precomputed read model (refreshed hourly) instead of
    aggregating ~1M trips per request: 12 s → a few ms."""
    window = 7 if days <= 7 else 30

    async def load() -> dict[str, Any]:
        async with db.tenant_tx(p.tenant_id) as conn:
            stats = await conn.fetchrow(
                """SELECT count(*) AS drivers, round(avg(score), 1) AS avg_score, max(refreshed_at) AS refreshed_at
                   FROM driver_safety WHERE days = $1""", window)
            dist = await conn.fetch(
                """SELECT least(9, floor(score / 10))::int AS b, count(*) AS n FROM driver_safety
                   WHERE days = $1 GROUP BY 1 ORDER BY 1""", window)
            recs = db.rows(await conn.fetch(
                """SELECT s.driver_id, s.trips, s.km, s.harsh_brakes, s.harsh_accels, s.overspeed_s, s.idle_s, s.score,
                          d.full_name
                   FROM driver_safety s JOIN driver d ON d.driver_id = s.driver_id
                   WHERE s.days = $1 ORDER BY s.score, s.driver_id LIMIT $2""", window, limit))
        pii = p.can(rbac.DRIVER_PII)
        for r in recs:
            name = r.pop("full_name")
            r["name"] = name if pii else pseudonym(r["driver_id"], ctx.settings.cursor_secret)
            r["harsh_per_100km"] = round((r["harsh_brakes"] + r["harsh_accels"]) * 100 / float(r["km"]), 2)
        counts = {r["b"]: r["n"] for r in dist}
        s = db.row(stats) or {}
        return {"days": window, "drivers": s.get("drivers", 0), "avg_score": s.get("avg_score"),
                "refreshed_at": s.get("refreshed_at"), "riskiest": recs,
                "distribution": [{"score_from": i * 10, "drivers": counts.get(i, 0)} for i in range(10)]}

    key = f"drivers:{window}:{limit}:{int(p.can(rbac.DRIVER_PII))}"
    return await ctx.cache.get_or_set(p.tenant_id or "", key, 120, load)


async def data_quality(ctx: AppContext, p: Principal, hours: int) -> list[dict[str, Any]]:
    sql = """
SELECT oem, replaceRegexpAll(reason, '[0-9]{6,}', 'N') AS reason, count() AS rejects, max(ts) AS last_seen
FROM fleet.ingest_rejects WHERE ts >= now() - toIntervalHour({hours:UInt32})
GROUP BY oem, reason ORDER BY rejects DESC LIMIT 50"""
    return await ctx.ch.query(sql, p.tenant_id or "platform", {"hours": hours})
