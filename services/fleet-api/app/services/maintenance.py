"""Predictive maintenance read model and work-order use cases."""

from __future__ import annotations

from datetime import date
from typing import Any

from app.context import AppContext
from app.domain.pagination import decode_cursor, encode_cursor
from app.infra import db
from app.security.jwt import Principal
from app.services.sqlbuild import Where

RISK_SQL = """
SELECT r.vin, r.risk_7d, r.top_component, r.top_factors, r.est_breakdown_cost, r.expected_savings_usd,
       r.scored_on, r.model_version, v.plate, f.name AS fleet_name, m.name AS model, m.powertrain,
       EXISTS (SELECT 1 FROM work_order w WHERE w.vin = r.vin
               AND w.status IN ('OPEN', 'SCHEDULED', 'IN_PROGRESS')) AS has_open_work_order
FROM vehicle_risk_current r
JOIN vehicle v ON v.vin = r.vin
JOIN fleet f ON f.fleet_id = v.fleet_id
JOIN vehicle_model m ON m.model_id = v.model_id
{where}
ORDER BY r.risk_7d DESC, r.vin DESC
LIMIT {limit}"""


async def risk_list(ctx: AppContext, p: Principal, *, cursor: str | None, limit: int, min_risk: float,
                    component: str | None, fleet_id: str | None) -> dict[str, Any]:
    c = decode_cursor(cursor, ctx.settings.cursor_secret)
    w = Where().add("r.risk_7d >= {}", min_risk)
    w.add_if(bool(component), "r.top_component = {}", component)
    w.add_if(bool(fleet_id), "v.fleet_id = {}::uuid", fleet_id)
    if c:
        w.add("(r.risk_7d, r.vin) < ({}::real, {})", c["risk"], c["vin"])
    async with db.tenant_tx(p.tenant_id) as conn:
        recs = db.rows(await conn.fetch(RISK_SQL.format(where=w.sql(), limit=limit + 1), *w.args))
    more = len(recs) > limit
    recs = recs[:limit]
    nxt = encode_cursor({"risk": recs[-1]["risk_7d"], "vin": recs[-1]["vin"]},
                        ctx.settings.cursor_secret) if more and recs else None
    return {"items": recs, "next_cursor": nxt}


async def risk_summary(ctx: AppContext, p: Principal) -> dict[str, Any]:
    async def load() -> dict[str, Any]:
        async with db.tenant_tx(p.tenant_id) as conn:
            by_comp = await conn.fetch(
                """SELECT top_component AS component, count(*) FILTER (WHERE risk_7d >= 0.5) AS high,
                          count(*) FILTER (WHERE risk_7d >= 0.25) AS elevated,
                          round(sum(risk_7d)::numeric, 1) AS expected_failures_7d,
                          round(sum(expected_savings_usd) FILTER (WHERE risk_7d >= 0.25), 0) AS savings_usd
                   FROM vehicle_risk_current GROUP BY top_component ORDER BY expected_failures_7d DESC""")
            bands = await conn.fetch(
                """SELECT width_bucket(risk_7d, 0, 1, 10) AS bucket, count(*) AS n
                   FROM vehicle_risk_current GROUP BY 1 ORDER BY 1""")
            model = await conn.fetchrow(
                "SELECT model_version, trained_at, algorithm, metrics FROM model_version WHERE is_active")
            history = await conn.fetch(
                """SELECT date_trunc('week', occurred_at)::date AS week, count(*) AS breakdowns,
                          round(sum(cost_usd), 0) AS cost_usd
                   FROM service_event WHERE event_type = 'BREAKDOWN'
                   GROUP BY 1 ORDER BY 1""")
        return {
            "by_component": db.rows(by_comp),
            "distribution": [{"risk_from": (r["bucket"] - 1) / 10, "risk_to": r["bucket"] / 10, "vehicles": r["n"]}
                             for r in bands],
            "model": db.row(model),
            "breakdown_history": [{**r, "week": r["week"].isoformat()} for r in db.rows(history)],
        }

    return await ctx.cache.get_or_set(p.tenant_id or "", "risk-summary", 60, load)


WO_COLS = """work_order_id, vin, source, component, priority, status, due_on, est_cost_usd, notes, created_by,
             created_at, updated_at, version"""


async def list_work_orders(ctx: AppContext, p: Principal, *, cursor: str | None, limit: int,
                           status: str | None) -> dict[str, Any]:
    c = decode_cursor(cursor, ctx.settings.cursor_secret)
    w = Where().add("tenant_id = {}::uuid", p.tenant_id)
    w.add_if(bool(status), "status = {}", status)
    if c:
        w.add("(created_at, work_order_id) < ({}::timestamptz, {}::uuid)", c["ts"], c["id"])
    sql = f"SELECT {WO_COLS} FROM work_order {w.sql()} ORDER BY created_at DESC, work_order_id DESC LIMIT {limit + 1}"
    async with db.tenant_tx(p.tenant_id) as conn:
        recs = db.rows(await conn.fetch(sql, *w.args))
    more = len(recs) > limit
    recs = recs[:limit]
    nxt = encode_cursor({"ts": recs[-1]["created_at"].isoformat(), "id": recs[-1]["work_order_id"]},
                        ctx.settings.cursor_secret) if more and recs else None
    return {"items": recs, "next_cursor": nxt}


class ConflictError(Exception):
    pass


async def create_work_order(ctx: AppContext, p: Principal, *, vin: str, source: str, component: str | None,
                            priority: str, due_on: date | None, notes: str | None,
                            est_cost_usd: float | None = None) -> dict[str, Any]:
    async with db.tenant_tx(p.tenant_id, readonly=False) as conn:
        exists = await conn.fetchval("SELECT 1 FROM vehicle WHERE vin = $1", vin)
        if not exists:
            raise LookupError(vin)
        if est_cost_usd is None and component:
            est_cost_usd = await conn.fetchval(
                "SELECT est_breakdown_cost * 0.25 FROM vehicle_risk_current WHERE vin = $1", vin)
        rec = await conn.fetchrow(
            f"""INSERT INTO work_order (tenant_id, vin, source, component, priority, due_on, est_cost_usd, notes,
                                        created_by)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING {WO_COLS}""",
            p.tenant_id, vin, source, component, priority, due_on, est_cost_usd, notes, p.user_id)
    await ctx.cache.invalidate(p.tenant_id or "", "summary")
    return db.row(rec)


async def update_work_order(ctx: AppContext, p: Principal, wo_id: str, *, expected_version: int,
                            status: str | None, notes: str | None, due_on: date | None) -> dict[str, Any]:
    """Optimistic concurrency: the update applies only if the caller saw the
    latest version (If-Match), otherwise 409 and the client re-reads."""
    async with db.tenant_tx(p.tenant_id, readonly=False) as conn:
        rec = await conn.fetchrow(
            f"""UPDATE work_order SET status = coalesce($3, status), notes = coalesce($4, notes),
                       due_on = coalesce($5, due_on), updated_at = now(), version = version + 1
                WHERE work_order_id = $1 AND version = $2 RETURNING {WO_COLS}""",
            wo_id, expected_version, status, notes, due_on)
        if rec is None:
            current = await conn.fetchval("SELECT version FROM work_order WHERE work_order_id = $1", wo_id)
            if current is None:
                raise LookupError(wo_id)
            raise ConflictError(f"version mismatch: current={current}")
    await ctx.cache.invalidate(p.tenant_id or "", "summary")
    return db.row(rec)
