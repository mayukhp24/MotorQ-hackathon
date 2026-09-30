"""Audit trail, data-subject erasure and platform operations."""

from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, Query, Request, status

from app.api.deps import get_ctx, require
from app.api.schemas import ErasureRequest
from app.context import AppContext
from app.domain.pagination import clamp_limit
from app.infra import db
from app.infra.audit import AuditEvent
from app.security import rbac
from app.security.jwt import Principal
from app.services import compliance as svc

router = APIRouter(prefix="/api/v1", tags=["compliance"])


@router.get("/compliance/audit", summary="Tamper-evident audit trail (keyset pagination)")
async def audit(cursor: str | None = None, limit: int = Query(50, ge=1, le=200),
                actor_type: str | None = Query(None, pattern="^(USER|AGENT|SYSTEM|SERVICE)$"),
                action: str | None = Query(None, max_length=60),
                p: Principal = Depends(require(rbac.AUDIT_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    await ctx.audit.flush()  # include this session's most recent events
    return await svc.audit_log(ctx, p, cursor=cursor, limit=clamp_limit(limit), actor_type=actor_type, action=action)


@router.get("/compliance/audit/verify", summary="Verify the audit hash chain")
async def verify(p: Principal = Depends(require(rbac.AUDIT_READ))) -> dict:
    return await svc.verify_chain(p)


@router.get("/compliance/drivers", summary="Find drivers (data subjects)")
async def drivers(q: str | None = Query(None, max_length=60), limit: int = Query(25, ge=1, le=100),
                  p: Principal = Depends(require(rbac.PRIVACY_ERASE)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return {"items": await svc.search_drivers(ctx, p, q, limit)}


@router.post("/compliance/erasure-requests", status_code=201, summary="Erase a driver's personal data")
async def erase(body: ErasureRequest, request: Request, p: Principal = Depends(require(rbac.PRIVACY_ERASE)),
                ctx: AppContext = Depends(get_ctx)) -> dict:
    try:
        rec = await svc.erase_driver(ctx, p, body.driver_id, body.reason)
    except LookupError as e:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "driver not found") from e
    except svc.AlreadyErased as e:
        raise HTTPException(status.HTTP_409_CONFLICT, "driver already erased") from e
    ctx.audit.record(AuditEvent(action="privacy.erasure", resource_type="driver", tenant_id=p.tenant_id,
                                actor_id=p.user_id, resource_id=body.driver_id,
                                request_id=getattr(request.state, "request_id", None),
                                details={"request_id": rec["request_id"], "reason": body.reason}))
    return rec


@router.get("/compliance/erasure-requests")
async def erasures(limit: int = Query(50, ge=1, le=200), p: Principal = Depends(require(rbac.PRIVACY_ERASE))) -> dict:
    return {"items": await svc.list_erasures(p, limit)}


@router.get("/admin/tenants", tags=["admin"], summary="Tenants overview (platform operators)")
async def tenants(p: Principal = Depends(require(rbac.PLATFORM_ADMIN))) -> dict:
    async with db.tenant_tx(None) as conn:
        return {"items": db.rows(await conn.fetch("SELECT * FROM platform_tenant_overview()"))}


@router.get("/admin/dependencies", tags=["admin"], summary="Dependency health and circuit breakers")
async def dependencies(p: Principal = Depends(require(rbac.FLEET_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    out: dict = {}
    try:
        async with db.tenant_tx(p.tenant_id) as conn:
            await conn.fetchval("SELECT 1")
        out["postgres"] = "up"
    except Exception:
        out["postgres"] = "down"
    for name, client in (("redis", ctx.redis), ("live_redis", ctx.live)):
        try:
            await client.ping()
            out[name] = "up"
        except Exception:
            out[name] = "down"
    out["clickhouse"] = "up" if await ctx.ch.ping() else "down"
    out["breakers"] = {"clickhouse": ctx.ch.breaker.state,
                       "llm": ctx.copilot.breaker.state if ctx.copilot else "n/a"}
    out["copilot_mode"] = "llm" if ctx.copilot and ctx.copilot.client else "offline"
    return out
