"""Copilot chat and human-in-the-loop approval of proposed actions."""

from __future__ import annotations

from dataclasses import asdict

from fastapi import APIRouter, Depends, HTTPException, Path, Query, Request, status

from app.api.deps import get_ctx, require
from app.api.schemas import UUID_PATTERN, ChatRequest
from app.context import AppContext
from app.infra import db
from app.infra.audit import AuditEvent
from app.security import rbac
from app.security.jwt import Principal
from app.services import maintenance as maint_svc

router = APIRouter(prefix="/api/v1/copilot", tags=["copilot"])


@router.post("/chat", summary="Ask the maintenance copilot")
async def chat(body: ChatRequest, request: Request, p: Principal = Depends(require(rbac.COPILOT_USE)),
               ctx: AppContext = Depends(get_ctx)) -> dict:
    assert ctx.copilot is not None
    decision = await ctx.limiter.hit(f"copilot:{p.user_id}", 30)  # LLM calls are costly: tighter limit
    if not decision.allowed:
        raise HTTPException(status.HTTP_429_TOO_MANY_REQUESTS, "copilot rate limit exceeded",
                            headers={"Retry-After": str(decision.reset_s)})
    try:
        res = await ctx.copilot.chat(p, body.message, body.conversation_id, getattr(request.state, "request_id", None))
    except PermissionError as e:
        raise HTTPException(status.HTTP_403_FORBIDDEN, str(e)) from e
    out = asdict(res)
    for c in out["tool_calls"]:  # keep the payload light; full results are in the audit trail
        c["result_preview"] = str(c.pop("result"))[:400]
    return out


@router.get("/actions", summary="Agent-proposed actions")
async def actions(status_: str = Query("PROPOSED", alias="status", pattern="^(PROPOSED|APPROVED|REJECTED|EXECUTED|FAILED)$"),
                  limit: int = Query(50, ge=1, le=200), p: Principal = Depends(require(rbac.COPILOT_USE)),
                  ctx: AppContext = Depends(get_ctx)) -> dict:
    async with db.tenant_tx(p.tenant_id) as conn:
        recs = await conn.fetch(
            """SELECT a.action_id, a.tool, a.arguments, a.rationale, a.status, a.result, a.created_at, a.decided_at,
                      u.full_name AS requested_by
               FROM agent_action a JOIN app_user u ON u.user_id = a.user_id
               WHERE a.status = $1 ORDER BY a.created_at DESC LIMIT $2""", status_, limit)
    return {"items": db.rows(recs)}


@router.post("/actions/{action_id}/{decision}", summary="Approve or reject a proposed action")
async def decide(request: Request, action_id: str = Path(pattern=UUID_PATTERN),
                 decision: str = Path(pattern="^(approve|reject)$"),
                 p: Principal = Depends(require(rbac.COPILOT_APPROVE)), ctx: AppContext = Depends(get_ctx)) -> dict:
    async with db.tenant_tx(p.tenant_id, readonly=False) as conn:
        act = await conn.fetchrow("SELECT * FROM agent_action WHERE action_id = $1 FOR UPDATE", action_id)
        if act is None:
            raise HTTPException(status.HTTP_404_NOT_FOUND, "action not found")
        if act["status"] != "PROPOSED":
            raise HTTPException(status.HTTP_409_CONFLICT, f"action already {act['status']}")
        if decision == "reject":
            await conn.execute("""UPDATE agent_action SET status = 'REJECTED', decided_by = $2, decided_at = now()
                                  WHERE action_id = $1""", action_id, p.user_id)
    result: dict | None = None
    new_status = "REJECTED"
    if decision == "approve":
        args = act["arguments"]
        try:
            wo = await maint_svc.create_work_order(ctx, p, vin=args["vin"], source="COPILOT",
                                                   component=args.get("component"), priority=args.get("priority", "P2"),
                                                   due_on=None, notes=f"Copilot proposal: {args.get('reason', '')}")
            result, new_status = {"work_order_id": wo["work_order_id"]}, "EXECUTED"
        except Exception as e:  # recorded, not hidden
            result, new_status = {"error": type(e).__name__}, "FAILED"
        async with db.tenant_tx(p.tenant_id, readonly=False) as conn:
            await conn.execute("""UPDATE agent_action SET status = $2, result = $3, decided_by = $4, decided_at = now()
                                  WHERE action_id = $1""", action_id, new_status, result, p.user_id)
    ctx.audit.record(AuditEvent(action=f"copilot.action.{decision}", resource_type="agent_action",
                                tenant_id=p.tenant_id, actor_id=p.user_id, resource_id=action_id,
                                request_id=getattr(request.state, "request_id", None),
                                details={"status": new_status, "result": result, "tool": act["tool"]}))
    return {"action_id": action_id, "status": new_status, "result": result}
