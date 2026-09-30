"""Alerts, maintenance, analytics endpoints."""

from __future__ import annotations

from fastapi import APIRouter, Depends, Header, HTTPException, Path, Query, status

from app.api.deps import get_ctx, require
from app.api.schemas import UUID_PATTERN, VIN_PATTERN, WorkOrderCreate, WorkOrderPatch
from app.context import AppContext
from app.domain.pagination import clamp_limit
from app.infra.breaker import CircuitOpenError
from app.security import rbac
from app.security.jwt import Principal
from app.services import alerts as alert_svc
from app.services import analytics as analytics_svc
from app.services import maintenance as maint_svc

router = APIRouter(prefix="/api/v1")

SEVERITY = "^(CRITICAL|HIGH|MEDIUM|LOW)$"


# -------------------------------------------------------------------- alerts
@router.get("/alerts", tags=["alerts"], summary="Alerts, newest first (keyset pagination)")
async def list_alerts(cursor: str | None = None, limit: int = Query(50, ge=1, le=200),
                      status_: str | None = Query(None, alias="status", pattern="^(OPEN|ACKNOWLEDGED|RESOLVED)$"),
                      severity: str | None = Query(None, pattern=SEVERITY),
                      type_: str | None = Query(None, alias="type", max_length=40),
                      vin: str | None = Query(None, pattern=VIN_PATTERN),
                      p: Principal = Depends(require(rbac.ALERT_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return await alert_svc.list_alerts(ctx, p, cursor=cursor, limit=clamp_limit(limit), status=status_,
                                       severity=severity, alert_type=type_, vin=vin)


@router.post("/alerts/{alert_id}/ack", tags=["alerts"], summary="Acknowledge an open alert")
async def ack_alert(alert_id: str = Path(pattern=UUID_PATTERN), p: Principal = Depends(require(rbac.ALERT_ACK)),
                    ctx: AppContext = Depends(get_ctx)) -> dict:
    res = await alert_svc.transition(ctx, p, alert_id, "ack")
    if res is None:
        raise HTTPException(status.HTTP_409_CONFLICT, "alert not found or not open")
    return res


@router.post("/alerts/{alert_id}/resolve", tags=["alerts"], summary="Resolve an alert")
async def resolve_alert(alert_id: str = Path(pattern=UUID_PATTERN), p: Principal = Depends(require(rbac.ALERT_ACK)),
                        ctx: AppContext = Depends(get_ctx)) -> dict:
    res = await alert_svc.transition(ctx, p, alert_id, "resolve")
    if res is None:
        raise HTTPException(status.HTTP_409_CONFLICT, "alert not found or already resolved")
    return res


# --------------------------------------------------------------- maintenance
@router.get("/maintenance/risk", tags=["maintenance"], summary="Vehicles ranked by 7-day breakdown risk")
async def risk(cursor: str | None = None, limit: int = Query(50, ge=1, le=200), min_risk: float = Query(0.2, ge=0, le=1),
               component: str | None = Query(None, pattern="^(cooling|battery12v|misfire|ev_pack)$"),
               fleet_id: str | None = Query(None, pattern=UUID_PATTERN),
               p: Principal = Depends(require(rbac.MAINT_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return await maint_svc.risk_list(ctx, p, cursor=cursor, limit=clamp_limit(limit), min_risk=min_risk,
                                     component=component, fleet_id=fleet_id)


@router.get("/maintenance/summary", tags=["maintenance"], summary="Risk by component, model metrics, history")
async def risk_summary(p: Principal = Depends(require(rbac.MAINT_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return await maint_svc.risk_summary(ctx, p)


@router.get("/maintenance/work-orders", tags=["maintenance"])
async def work_orders(cursor: str | None = None, limit: int = Query(50, ge=1, le=200),
                      status_: str | None = Query(None, alias="status",
                                                  pattern="^(OPEN|SCHEDULED|IN_PROGRESS|DONE|CANCELLED)$"),
                      p: Principal = Depends(require(rbac.MAINT_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return await maint_svc.list_work_orders(ctx, p, cursor=cursor, limit=clamp_limit(limit), status=status_)


@router.post("/maintenance/work-orders", tags=["maintenance"], status_code=201)
async def create_work_order(body: WorkOrderCreate, p: Principal = Depends(require(rbac.MAINT_WRITE)),
                            ctx: AppContext = Depends(get_ctx)) -> dict:
    try:
        return await maint_svc.create_work_order(ctx, p, vin=body.vin, source=body.source, component=body.component,
                                                 priority=body.priority, due_on=body.due_on, notes=body.notes)
    except LookupError as e:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "vehicle not found") from e


@router.patch("/maintenance/work-orders/{work_order_id}", tags=["maintenance"],
              summary="Update a work order (optimistic concurrency via If-Match: <version>)")
async def patch_work_order(body: WorkOrderPatch, work_order_id: str = Path(pattern=UUID_PATTERN),
                           if_match: str = Header(..., alias="If-Match"),
                           p: Principal = Depends(require(rbac.MAINT_WRITE)), ctx: AppContext = Depends(get_ctx)) -> dict:
    try:
        version = int(if_match.strip('"W/ '))
    except ValueError as e:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "If-Match must carry the work order version") from e
    try:
        return await maint_svc.update_work_order(ctx, p, work_order_id, expected_version=version, status=body.status,
                                                 notes=body.notes, due_on=body.due_on)
    except LookupError as e:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "work order not found") from e
    except maint_svc.ConflictError as e:
        raise HTTPException(status.HTTP_409_CONFLICT, str(e)) from e


# ----------------------------------------------------------------- analytics
@router.get("/analytics/fleet-hourly", tags=["analytics"], summary="Hourly activity rollup (ClickHouse)")
async def fleet_hourly(hours: int = Query(24, ge=1, le=24 * 30), p: Principal = Depends(require(rbac.ANALYTICS_READ)),
                       ctx: AppContext = Depends(get_ctx)) -> dict:
    try:
        return {"hours": hours, "points": await analytics_svc.fleet_hourly(ctx, p, hours), "degraded": False}
    except (CircuitOpenError, RuntimeError, OSError):
        return {"hours": hours, "points": [], "degraded": True}


@router.get("/analytics/alert-trend", tags=["analytics"])
async def alert_trend(days: int = Query(14, ge=1, le=90), p: Principal = Depends(require(rbac.ANALYTICS_READ)),
                      ctx: AppContext = Depends(get_ctx)) -> dict:
    return {"days": days, "points": await alert_svc.alert_trend(ctx, p, days)}


@router.get("/analytics/idle-cost", tags=["analytics"], summary="Idling hours and $ cost (ClickHouse)")
async def idle_cost(days: int = Query(7, ge=1, le=60), p: Principal = Depends(require(rbac.ANALYTICS_READ)),
                    ctx: AppContext = Depends(get_ctx)) -> dict:
    try:
        return await analytics_svc.idle_cost(ctx, p, days)
    except (CircuitOpenError, RuntimeError, OSError) as e:
        raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, "analytics store unavailable") from e


@router.get("/analytics/driver-safety", tags=["analytics"], summary="Driver safety scores (pseudonymised)")
async def driver_safety(days: int = Query(7, ge=1, le=30), limit: int = Query(20, ge=1, le=100),
                        p: Principal = Depends(require(rbac.ANALYTICS_READ)), ctx: AppContext = Depends(get_ctx)) -> dict:
    return await analytics_svc.driver_safety(ctx, p, days, limit)


@router.get("/analytics/data-quality", tags=["analytics"], summary="Ingestion rejects by OEM and reason (platform)")
async def data_quality(hours: int = Query(24, ge=1, le=336), p: Principal = Depends(require(rbac.PLATFORM_ADMIN)),
                       ctx: AppContext = Depends(get_ctx)) -> dict:
    return {"hours": hours, "items": await analytics_svc.data_quality(ctx, p, hours)}
