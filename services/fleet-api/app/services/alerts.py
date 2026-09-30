"""Alert queries and state transitions."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from app.context import AppContext
from app.domain.pagination import decode_cursor, encode_cursor
from app.infra import db
from app.security import rbac
from app.security.jwt import Principal
from app.security.masking import mask_location
from app.services.sqlbuild import Where

LIST_SQL = """
SELECT a.alert_id, a.vin, a.alert_type, a.severity, a.title, a.status, a.ts, a.detected_at, a.lat, a.lon,
       a.details, a.acknowledged_at, a.resolved_at, v.plate, f.name AS fleet_name
FROM alert a
JOIN vehicle v ON v.vin = a.vin
JOIN fleet f ON f.fleet_id = v.fleet_id
{where}
ORDER BY a.ts DESC, a.alert_id DESC
LIMIT {limit}"""


async def list_alerts(ctx: AppContext, p: Principal, *, cursor: str | None, limit: int, status: str | None,
                      severity: str | None, alert_type: str | None, vin: str | None) -> dict[str, Any]:
    c = decode_cursor(cursor, ctx.settings.cursor_secret)
    w = Where().add("a.tenant_id = {}::uuid", p.tenant_id)
    w.add_if(bool(status), "a.status = {}", status)
    w.add_if(bool(severity), "a.severity = {}", severity)
    w.add_if(bool(alert_type), "a.alert_type = {}", alert_type)
    w.add_if(bool(vin), "a.vin = {}", vin)
    if c:
        w.add("(a.ts, a.alert_id) < ({}::timestamptz, {}::uuid)", datetime.fromisoformat(c["ts"]), c["id"])
    async with db.tenant_tx(p.tenant_id) as conn:
        recs = db.rows(await conn.fetch(LIST_SQL.format(where=w.sql(), limit=limit + 1), *w.args))
    more = len(recs) > limit
    recs = recs[:limit]
    precise = p.can(rbac.LOCATION_PRECISE)
    for r in recs:
        r["lat"], r["lon"] = mask_location(r["lat"], r["lon"], precise)
    nxt = None
    if more and recs:
        nxt = encode_cursor({"ts": recs[-1]["ts"].isoformat(), "id": recs[-1]["alert_id"]}, ctx.settings.cursor_secret)
    return {"items": recs, "next_cursor": nxt}


async def transition(ctx: AppContext, p: Principal, alert_id: str, action: str) -> dict[str, Any] | None:
    if action == "ack":
        sql = """UPDATE alert SET status = 'ACKNOWLEDGED', acknowledged_by = $2, acknowledged_at = now()
                 WHERE alert_id = $1 AND status = 'OPEN' RETURNING alert_id, status, acknowledged_at, resolved_at"""
        args: tuple[Any, ...] = (alert_id, p.user_id)
    else:
        sql = """UPDATE alert SET status = 'RESOLVED', resolved_at = now(),
                        acknowledged_by = coalesce(acknowledged_by, $2), acknowledged_at = coalesce(acknowledged_at, now())
                 WHERE alert_id = $1 AND status <> 'RESOLVED' RETURNING alert_id, status, acknowledged_at, resolved_at"""
        args = (alert_id, p.user_id)
    async with db.tenant_tx(p.tenant_id, readonly=False) as conn:
        row = await conn.fetchrow(sql, *args)
    if row:
        await ctx.cache.invalidate(p.tenant_id or "", "summary")
        return db.row(row)
    return None


async def alert_trend(ctx: AppContext, p: Principal, days: int) -> list[dict[str, Any]]:
    """Completed days come from the mv_alert_daily read model; only days not
    yet materialised are aggregated live (a small index range)."""
    async def load() -> list[dict[str, Any]]:
        async with db.tenant_tx(p.tenant_id) as conn:
            recs = await conn.fetch(
                """WITH hist AS (SELECT day, severity, n FROM alert_daily WHERE day >= current_date - $2::int),
                        edge AS (SELECT coalesce(max(day) + 1, current_date - $2::int) AS d FROM alert_daily)
                   SELECT day, severity, n FROM hist
                   UNION ALL
                   SELECT (a.ts AT TIME ZONE 'UTC')::date, a.severity, count(*)::int
                   FROM alert a, edge
                   WHERE a.tenant_id = $1::uuid AND a.ts >= edge.d::timestamp AT TIME ZONE 'UTC'
                   GROUP BY 1, 2
                   ORDER BY 1, 2""", p.tenant_id, days)
        return [{"day": r["day"].isoformat(), "severity": r["severity"], "count": r["n"]} for r in recs]

    return await ctx.cache.get_or_set(p.tenant_id or "", f"alert-trend:{days}", 60, load)
