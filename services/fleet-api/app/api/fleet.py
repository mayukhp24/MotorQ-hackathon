"""Fleet, vehicles and live-map endpoints (+ WebSocket stream)."""

from __future__ import annotations

import asyncio
import contextlib
import secrets

import orjson
from fastapi import APIRouter, Depends, HTTPException, Query, WebSocket, WebSocketDisconnect, status

from app.api.deps import get_ctx, principal_from_token, require
from app.api.schemas import VIN_PATTERN
from app.context import AppContext
from app.domain.pagination import clamp_limit
from app.infra.breaker import CircuitOpenError
from app.security import rbac
from app.security.jwt import Principal
from app.security.masking import mask_location
from app.services import fleet as svc
from app.services import live as live_svc

router = APIRouter(prefix="/api/v1", tags=["fleet"])


@router.get("/fleet/summary", summary="Fleet KPIs (live + relational)")
async def fleet_summary(p: Principal = Depends(require(rbac.FLEET_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return await svc.fleet_summary(ctx, p)


@router.get("/vehicles", summary="List vehicles (keyset pagination)")
async def vehicles(cursor: str | None = None, limit: int = Query(50, ge=1, le=200), fleet_id: str | None = None,
                   powertrain: str | None = Query(None, pattern="^(ICE|EV|HEV)$"), q: str | None = Query(None, max_length=17),
                   risk_min: float | None = Query(None, ge=0, le=1),
                   p: Principal = Depends(require(rbac.VEHICLE_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return await svc.list_vehicles(ctx, p, cursor=cursor, limit=clamp_limit(limit), fleet_id=fleet_id,
                                   powertrain=powertrain, q=q, risk_min=risk_min)


@router.get("/vehicles/{vin}", summary="Vehicle detail")
async def vehicle(vin: str, p: Principal = Depends(require(rbac.VEHICLE_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    _check_vin(vin)
    v = await svc.vehicle_detail(ctx, p, vin)
    if v is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "vehicle not found")
    return v


@router.get("/vehicles/{vin}/telemetry", summary="Down-sampled telemetry history (ClickHouse)")
async def vehicle_telemetry(vin: str, minutes: int = Query(120, ge=5, le=7 * 24 * 60),
                            p: Principal = Depends(require(rbac.VEHICLE_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    _check_vin(vin)
    try:
        return {"vin": vin, "points": await svc.vehicle_telemetry(ctx, p, vin, minutes), "degraded": False}
    except (CircuitOpenError, RuntimeError, OSError):
        # Graceful degradation: history unavailable, live state still served elsewhere.
        return {"vin": vin, "points": [], "degraded": True}


@router.get("/vehicles/{vin}/daily", summary="Daily health features (feature store)")
async def vehicle_daily(vin: str, days: int = Query(30, ge=1, le=90), p: Principal = Depends(require(rbac.VEHICLE_READ)),
                        ctx: AppContext = Depends(get_ctx)) -> dict:
    _check_vin(vin)
    try:
        return {"vin": vin, "days": await svc.vehicle_daily(ctx, p, vin, days), "degraded": False}
    except (CircuitOpenError, RuntimeError, OSError):
        return {"vin": vin, "days": [], "degraded": True}


def _check_vin(vin: str) -> None:
    import re

    if not re.fullmatch(VIN_PATTERN, vin):
        raise HTTPException(status.HTTP_422_UNPROCESSABLE_ENTITY, "invalid VIN")


# ------------------------------------------------------------------ live map
@router.get("/live/clusters", summary="Vehicle density by geohash cell")
async def live_clusters(precision: int = Query(4, ge=3, le=5), p: Principal = Depends(require(rbac.FLEET_READ)),
                        ctx: AppContext = Depends(get_ctx)) -> dict:
    return {"precision": precision,
            "cells": await live_svc.clusters(ctx.live, p.tenant_id or "", precision, ctx.settings.snapshot_stale_s)}


@router.get("/live/vehicles", summary="Vehicles inside a bounding box")
async def live_vehicles(bbox: str = Query(..., description="minLon,minLat,maxLon,maxLat"),
                        limit: int = Query(1000, ge=1, le=3000), p: Principal = Depends(require(rbac.VEHICLE_READ)),
                        ctx: AppContext = Depends(get_ctx)) -> dict:
    try:
        parts = tuple(float(x) for x in bbox.split(","))
        if len(parts) != 4:
            raise ValueError
    except ValueError as e:
        raise HTTPException(status.HTTP_422_UNPROCESSABLE_ENTITY, "bbox must be minLon,minLat,maxLon,maxLat") from e
    items = await live_svc.vehicles_in_box(ctx.live, p.tenant_id or "", parts, limit, p.can(rbac.LOCATION_PRECISE))
    return {"items": items, "masked": not p.can(rbac.LOCATION_PRECISE)}


@router.get("/live/top-dtc", summary="Trending fault codes (Count-Min Sketch top-K, last 15 min)")
async def live_top_dtc(p: Principal = Depends(require(rbac.FLEET_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return {"items": await live_svc.top_dtc(ctx.live, p.tenant_id or "", ctx.settings.snapshot_stale_s)}


@router.post("/live/ws-ticket", summary="One-time ticket for the WebSocket stream")
async def ws_ticket(p: Principal = Depends(require(rbac.FLEET_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    # Browsers cannot set headers on WebSocket upgrades; a 30 s single-use
    # ticket avoids putting the long-lived bearer token in a URL.
    ticket = secrets.token_urlsafe(24)
    await ctx.redis.set(f"wsticket:{ticket}", orjson.dumps({"u": p.user_id, "t": p.tenant_id, "r": list(p.roles)}),
                        ex=30)
    return {"ticket": ticket, "expires_in": 30}


@router.websocket("/live/stream")
async def live_stream(ws: WebSocket, ticket: str = Query(...)) -> None:
    ctx: AppContext = ws.app.state.ctx
    raw = await ctx.redis.getdel(f"wsticket:{ticket}")
    if not raw:
        await ws.close(code=4401)
        return
    info = orjson.loads(raw)
    perms = rbac.permissions_for(info["r"])
    precise = rbac.LOCATION_PRECISE in perms
    tenant = info["t"]
    await ws.accept()
    pubsub = ctx.live.pubsub()
    await pubsub.subscribe(f"alerts:{tenant}")

    async def push_kpis() -> None:
        while True:
            kpi = await live_svc.merged_kpi(ctx.live, tenant, ctx.settings.snapshot_stale_s)
            await ws.send_text(orjson.dumps({"type": "kpi", "data": kpi}).decode())
            await asyncio.sleep(2)

    async def push_alerts() -> None:
        while True:
            msg = await pubsub.get_message(ignore_subscribe_messages=True, timeout=1.0)
            if msg is None:
                continue
            alert = orjson.loads(msg["data"])
            alert["lat"], alert["lon"] = mask_location(alert.get("lat"), alert.get("lon"), precise)
            await ws.send_text(orjson.dumps({"type": "alert", "data": alert}).decode())

    tasks = [asyncio.create_task(push_kpis()), asyncio.create_task(push_alerts())]
    try:
        while True:
            await ws.receive_text()  # keep-alive / client pings; disconnect ends the loop
    except WebSocketDisconnect:
        pass
    finally:
        for t in tasks:
            t.cancel()
        with contextlib.suppress(Exception):
            await pubsub.unsubscribe()
            await pubsub.aclose()


__all__ = ["router", "principal_from_token"]
