"""Audit trail queries and the data-subject erasure flow (GDPR Art. 17,
India DPDP Act 2023 s.12)."""

from __future__ import annotations

import asyncio
from typing import Any

from app.context import AppContext
from app.domain.pagination import decode_cursor, encode_cursor
from app.infra import db
from app.security import rbac
from app.security.jwt import Principal
from app.security.masking import pseudonym
from app.services.sqlbuild import Where


async def audit_log(ctx: AppContext, p: Principal, *, cursor: str | None, limit: int, actor_type: str | None,
                    action: str | None) -> dict[str, Any]:
    c = decode_cursor(cursor, ctx.settings.cursor_secret)
    w = Where().add("tenant_id = {}::uuid", p.tenant_id)
    w.add_if(bool(actor_type), "actor_type = {}", actor_type)
    w.add_if(bool(action), "action ILIKE {}", f"%{action}%" if action else None)
    w.add_if(c is not None, "audit_id < {}", (c or {}).get("id"))
    sql = f"""SELECT audit_id, ts, actor_id, actor_type, action, resource_type, resource_id, outcome,
                     host(ip) AS ip, request_id, details, encode(hash, 'hex') AS hash
              FROM audit_log {w.sql()} ORDER BY audit_id DESC LIMIT {limit + 1}"""
    async with db.tenant_tx(p.tenant_id) as conn:
        recs = db.rows(await conn.fetch(sql, *w.args))
    more = len(recs) > limit
    recs = recs[:limit]
    nxt = encode_cursor({"id": recs[-1]["audit_id"]}, ctx.settings.cursor_secret) if more and recs else None
    return {"items": recs, "next_cursor": nxt}


async def verify_chain(p: Principal) -> dict[str, Any]:
    async with db.tenant_tx(p.tenant_id) as conn:
        broken = await conn.fetchval("SELECT audit_verify()")
        total = await conn.fetchval("SELECT count(*) FROM audit_log")
    return {"intact": broken is None, "first_broken_audit_id": broken, "tenant_rows": total}


async def search_drivers(ctx: AppContext, p: Principal, q: str | None, limit: int) -> list[dict[str, Any]]:
    pii = p.can(rbac.DRIVER_PII)
    w = Where().add("tenant_id = {}::uuid", p.tenant_id)
    if q:
        w.add("(full_name ILIKE {} OR driver_id::text LIKE {})", f"%{q}%", f"{q}%")
    key_param = w.param(ctx.settings.pii_encryption_key)
    sql = f"""SELECT driver_id, full_name, status, erased_at,
                     CASE WHEN status = 'ERASED' THEN NULL
                          ELSE right(pgp_sym_decrypt(license_no_enc, {key_param}), 4) END AS license_last4,
                     (SELECT count(*) FROM vehicle_assignment a WHERE a.driver_id = d.driver_id AND a.valid_to IS NULL)
                        AS active_assignments
              FROM driver d {w.sql()} ORDER BY full_name, driver_id LIMIT {limit}"""
    async with db.tenant_tx(p.tenant_id) as conn:
        recs = db.rows(await conn.fetch(sql, *w.args))
    for r in recs:
        if not pii:
            r["full_name"] = pseudonym(r["driver_id"], ctx.settings.cursor_secret) if r["status"] != "ERASED" else "[erased]"
            r["license_last4"] = None
    return recs


class AlreadyErased(Exception):
    pass


async def erase_driver(ctx: AppContext, p: Principal, driver_id: str, reason: str) -> dict[str, Any]:
    """Pseudonymise a driver everywhere they are identifiable, in one
    transaction, and return an evidence report. Telemetry in ClickHouse is
    keyed by VIN and holds no driver identifiers, so nothing to erase there;
    trips keep their operational metrics but lose the driver link and
    fine-grained locations."""
    async with db.tenant_tx(p.tenant_id, readonly=False) as conn:
        d = await conn.fetchrow("SELECT driver_id, status FROM driver WHERE driver_id = $1 FOR UPDATE", driver_id)
        if d is None:
            raise LookupError(driver_id)
        if d["status"] == "ERASED":
            raise AlreadyErased(driver_id)
        req_id = await conn.fetchval(
            """INSERT INTO erasure_request (tenant_id, subject_type, subject_id, requested_by, reason)
               VALUES ($1, 'DRIVER', $2, $3, $4) RETURNING request_id""", p.tenant_id, driver_id, p.user_id, reason)
        await conn.execute(
            """UPDATE driver SET full_name = '[erased]', license_hash = encode(gen_random_bytes(32), 'hex'),
                      license_no_enc = '\\x'::bytea, phone_enc = NULL, status = 'ERASED', erased_at = now(),
                      safety_opt_in = false
               WHERE driver_id = $1""", driver_id)
        trips = await conn.execute(
            """UPDATE trip SET driver_id = NULL, start_geohash = left(start_geohash, 4), end_geohash = left(end_geohash, 4)
               WHERE driver_id = $1""", driver_id)
        assignments = await conn.execute("DELETE FROM vehicle_assignment WHERE driver_id = $1", driver_id)
        report = {
            "postgres.driver": "identity fields overwritten, encrypted licence/phone destroyed",
            "postgres.trip": f"{int(trips.split()[-1])} trips unlinked from driver; locations coarsened to ~20 km",
            "postgres.vehicle_assignment": f"{int(assignments.split()[-1])} assignments removed",
            "clickhouse.telemetry": "no driver identifiers stored (keyed by VIN)",
            "redis.live_state": "no driver identifiers stored",
            "kafka.trips.v1": "driver_id ages out with topic retention (7 days)",
            "audit_log": "retained under legal-obligation exemption; references the pseudonymous driver_id only",
        }
        rec = await conn.fetchrow(
            """UPDATE erasure_request SET status = 'COMPLETED', completed_at = now(), report = $2
               WHERE request_id = $1 RETURNING request_id, subject_id, status, requested_at, completed_at, report""",
            req_id, report)
    asyncio.create_task(_refresh_derived(ctx, p.tenant_id or ""))
    return db.row(rec)


async def _refresh_derived(ctx: AppContext, tenant: str) -> None:
    """Propagate the erasure to derived read models (driver safety)."""
    try:
        async with db.pool().acquire() as conn:
            await conn.execute("SELECT refresh_read_models()")
        keys = [k async for k in ctx.redis.scan_iter(match=f"cache:{tenant}:drivers:*")]
        if keys:
            await ctx.redis.delete(*keys)
    except Exception:  # best effort; the hourly job refreshes anyway
        pass


async def list_erasures(p: Principal, limit: int) -> list[dict[str, Any]]:
    async with db.tenant_tx(p.tenant_id) as conn:
        return db.rows(await conn.fetch(
            """SELECT request_id, subject_type, subject_id, requested_by, reason, status, requested_at, completed_at,
                      report FROM erasure_request ORDER BY requested_at DESC LIMIT $1""", limit))
