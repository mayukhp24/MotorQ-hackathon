"""Tools the maintenance copilot may call.

Design rules (least privilege):
- Tools never accept a tenant: scope comes from the caller's verified token.
- Each tool declares the permission it needs; the caller's RBAC applies.
- Read tools return compact, masked data. The single write-like tool only
  *proposes* a work order; a human with `copilot:approve` must approve it.
"""

from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any

from app.agent.embeddings import embed, to_pgvector
from app.agent.guardrails import VIN_RE, clamp_float, clamp_int
from app.context import AppContext
from app.infra import db
from app.security import rbac
from app.security.jwt import Principal
from app.services import alerts as alert_svc
from app.services import analytics as analytics_svc
from app.services import fleet as fleet_svc
from app.services import maintenance as maint_svc

COMPONENTS = ["any", "cooling", "battery12v", "misfire", "ev_pack"]


@dataclass
class ToolContext:
    ctx: AppContext
    principal: Principal
    conversation_id: str


Handler = Callable[[ToolContext, dict[str, Any]], Awaitable[dict[str, Any]]]


@dataclass
class Tool:
    name: str
    description: str
    schema: dict[str, Any]
    permission: str
    handler: Handler
    kind: str = "read"  # read | propose

    def definition(self) -> dict[str, Any]:
        return {"name": self.name, "description": self.description, "strict": True, "input_schema": self.schema}


def _obj(props: dict[str, Any]) -> dict[str, Any]:
    return {"type": "object", "properties": props, "required": list(props), "additionalProperties": False}


async def _fleet_summary(tc: ToolContext, _: dict[str, Any]) -> dict[str, Any]:
    s = await fleet_svc.fleet_summary(tc.ctx, tc.principal)
    return {"vehicles": s["vehicles"], "live": s["live"], "open_alerts": s["open_alerts"], "risk": s["risk"],
            "open_work_orders": s["open_work_orders"]}


async def _at_risk(tc: ToolContext, a: dict[str, Any]) -> dict[str, Any]:
    comp = a.get("component") if a.get("component") in COMPONENTS[1:] else None
    res = await maint_svc.risk_list(tc.ctx, tc.principal, cursor=None, limit=clamp_int(a.get("limit"), 1, 20, 10),
                                    min_risk=clamp_float(a.get("min_risk"), 0, 1, 0.3), component=comp, fleet_id=None)
    return {"vehicles": [{
        "vin": r["vin"], "plate": r["plate"], "fleet": r["fleet_name"], "model": r["model"],
        "risk_7d": round(r["risk_7d"], 3), "component": r["top_component"],
        "top_factors": (r["top_factors"] or [])[:3], "est_breakdown_cost_usd": r["est_breakdown_cost"],
        "expected_savings_usd": r["expected_savings_usd"], "has_open_work_order": r["has_open_work_order"],
    } for r in res["items"]]}


async def _vehicle_health(tc: ToolContext, a: dict[str, Any]) -> dict[str, Any]:
    vin = str(a.get("vin", "")).strip().upper()
    if not VIN_RE.fullmatch(vin):
        return {"error": "invalid VIN format"}
    v = await fleet_svc.vehicle_detail(tc.ctx, tc.principal, vin)
    if v is None:
        return {"error": f"vehicle {vin} not found in your fleet"}
    live = v.get("live") or {}
    return {
        "vin": vin, "plate": v["plate"], "model": v["model"], "powertrain": v["powertrain"],
        "fleet": v["fleet_name"], "city": v["city"], "risk": v["risk"],
        "live": {k: live.get(k) for k in ("status", "speed_kmh", "coolant_c", "batt_v", "soc_pct", "fuel_pct",
                                          "pack_temp_c", "online", "critical")},
        "recent_alerts": [{"type": x["alert_type"], "severity": x["severity"], "title": x["title"],
                           "status": x["status"], "ts": str(x["ts"]),
                           "dtc": (x.get("details") or {}).get("dtc")} for x in v["recent_alerts"][:6]],
        "service_history": [{"type": s["event_type"], "component": s["component"], "at": str(s["occurred_at"])}
                            for s in v["service_history"][:4]],
        "open_work_orders": [w for w in v["work_orders"] if w["status"] in ("OPEN", "SCHEDULED", "IN_PROGRESS")],
    }


async def _search_alerts(tc: ToolContext, a: dict[str, Any]) -> dict[str, Any]:
    sev = a.get("severity") if a.get("severity") in ("CRITICAL", "HIGH", "MEDIUM", "LOW") else None
    status = a.get("status") if a.get("status") in ("OPEN", "ACKNOWLEDGED", "RESOLVED") else None
    res = await alert_svc.list_alerts(tc.ctx, tc.principal, cursor=None, limit=clamp_int(a.get("limit"), 1, 20, 10),
                                      status=status, severity=sev, alert_type=None, vin=None)
    return {"alerts": [{"vin": r["vin"], "plate": r["plate"], "type": r["alert_type"], "severity": r["severity"],
                        "title": r["title"], "status": r["status"], "ts": str(r["ts"])} for r in res["items"]]}


async def _knowledge(tc: ToolContext, a: dict[str, Any]) -> dict[str, Any]:
    q = str(a.get("query", ""))[:500]
    k = clamp_int(a.get("k"), 1, 5, 3)
    async with db.tenant_tx(tc.principal.tenant_id) as conn:
        recs = await conn.fetch(
            """SELECT dtc_code, component, title, content, source,
                      round((1 - (embedding <=> $1::vector))::numeric, 3) AS similarity
               FROM fault_knowledge WHERE embedding IS NOT NULL
               ORDER BY embedding <=> $1::vector LIMIT $2""", to_pgvector(embed(q)), k)
    return {"documents": db.rows(recs)}


async def _idle(tc: ToolContext, a: dict[str, Any]) -> dict[str, Any]:
    r = await analytics_svc.idle_cost(tc.ctx, tc.principal, clamp_int(a.get("days"), 1, 30, 7))
    return {"days": r["days"], "total": r["total"], "top_vehicles": r["top_vehicles"][:10]}


async def _drivers(tc: ToolContext, a: dict[str, Any]) -> dict[str, Any]:
    r = await analytics_svc.driver_safety(tc.ctx, tc.principal, clamp_int(a.get("days"), 1, 30, 7), 10)
    return {"drivers_scored": r["drivers"], "avg_score": r["avg_score"],
            "riskiest": [{k: d[k] for k in ("name", "score", "km", "harsh_per_100km", "overspeed_s", "trips")}
                         for d in r["riskiest"]]}


async def _propose_work_order(tc: ToolContext, a: dict[str, Any]) -> dict[str, Any]:
    vin = str(a.get("vin", "")).strip().upper()
    if not VIN_RE.fullmatch(vin):
        return {"error": "invalid VIN format"}
    comp = a.get("component") if a.get("component") in COMPONENTS[1:] else None
    prio = a.get("priority") if a.get("priority") in ("P1", "P2", "P3") else "P2"
    reason = str(a.get("reason", ""))[:500]
    args = {"vin": vin, "component": comp, "priority": prio, "reason": reason}
    async with db.tenant_tx(tc.principal.tenant_id, readonly=False) as conn:
        if not await conn.fetchval("SELECT 1 FROM vehicle WHERE vin = $1", vin):
            return {"error": f"vehicle {vin} not found in your fleet"}
        # One pending proposal per vehicle: repeated asks return the existing one
        # (the advisory lock serialises concurrent requests for the same VIN).
        await conn.execute("SELECT pg_advisory_xact_lock(hashtext('agent_action:' || $1))", vin)
        existing = await conn.fetchval(
            """SELECT action_id FROM agent_action WHERE status = 'PROPOSED' AND tool = 'create_work_order'
               AND arguments->>'vin' = $1 ORDER BY created_at LIMIT 1""", vin)
        if existing:
            return {"status": "ALREADY_PROPOSED", "action_id": str(existing),
                    "note": "A proposal for this vehicle is already awaiting approval."}
        action_id = await conn.fetchval(
            """INSERT INTO agent_action (tenant_id, user_id, conversation_id, tool, arguments, rationale)
               VALUES ($1, $2, $3, 'create_work_order', $4, $5) RETURNING action_id""",
            tc.principal.tenant_id, tc.principal.user_id, tc.conversation_id, args, reason)
    return {"status": "PROPOSED", "action_id": str(action_id),
            "note": "A maintenance manager must approve this action before a work order is created."}


TOOLS: list[Tool] = [
    Tool("get_fleet_summary", "Current fleet KPIs: vehicles online by status, open alerts by severity, predicted "
         "breakdown risk totals and open work orders.", _obj({}), rbac.FLEET_READ, _fleet_summary),
    Tool("list_at_risk_vehicles", "Vehicles most likely to break down in the next 7 days according to the "
         "predictive-maintenance model, with the component at risk, top contributing factors and $ impact. "
         "Use component 'any' for all components.",
         _obj({"min_risk": {"type": "number", "description": "0-1 probability threshold, e.g. 0.3"},
               "component": {"type": "string", "enum": COMPONENTS},
               "limit": {"type": "integer", "description": "1-20"}}), rbac.MAINT_READ, _at_risk),
    Tool("get_vehicle_health", "Full health picture for one vehicle by 17-character VIN: live signals, risk score, "
         "recent alerts/DTCs, service history and open work orders.",
         _obj({"vin": {"type": "string"}}), rbac.VEHICLE_READ, _vehicle_health),
    Tool("search_alerts", "Recent alerts, newest first. Use 'any' to not filter on a field.",
         _obj({"severity": {"type": "string", "enum": ["any", "CRITICAL", "HIGH", "MEDIUM", "LOW"]},
               "status": {"type": "string", "enum": ["any", "OPEN", "ACKNOWLEDGED", "RESOLVED"]},
               "limit": {"type": "integer", "description": "1-20"}}), rbac.ALERT_READ, _search_alerts),
    Tool("search_fault_knowledge", "Semantic search over the fault knowledge base (DTC meanings, likely causes, "
         "repair procedures, typical costs, past repair notes).",
         _obj({"query": {"type": "string"}, "k": {"type": "integer", "description": "1-5"}}),
         rbac.VEHICLE_READ, _knowledge),
    Tool("idle_cost_report", "Idling hours and estimated fuel/energy cost over the last N days, with the top idling "
         "vehicles and the saving from a 30% idle reduction.",
         _obj({"days": {"type": "integer", "description": "1-30"}}), rbac.ANALYTICS_READ, _idle),
    Tool("driver_safety_report", "Driver safety scores from harsh-driving and overspeed events per 100 km "
         "(driver names are pseudonymised unless the user may see personal data).",
         _obj({"days": {"type": "integer", "description": "1-30"}}), rbac.ANALYTICS_READ, _drivers),
    Tool("propose_work_order", "Propose a maintenance work order for a vehicle. This does NOT create it: the "
         "proposal is queued for human approval. Use only when the user asks to schedule/fix something.",
         _obj({"vin": {"type": "string"}, "component": {"type": "string", "enum": COMPONENTS},
               "priority": {"type": "string", "enum": ["P1", "P2", "P3"]},
               "reason": {"type": "string"}}), rbac.MAINT_WRITE, _propose_work_order, kind="propose"),
]
BY_NAME = {t.name: t for t in TOOLS}


def available_tools(p: Principal) -> list[Tool]:
    return [t for t in TOOLS if p.can(t.permission)]


async def run_tool(tc: ToolContext, name: str, args: dict[str, Any]) -> tuple[dict[str, Any], bool]:
    """Execute a tool with permission enforcement. Returns (result, is_error)."""
    tool = BY_NAME.get(name)
    if tool is None:
        return {"error": f"unknown tool {name}"}, True
    if not tc.principal.can(tool.permission):
        return {"error": f"permission '{tool.permission}' required"}, True
    try:
        result = await tool.handler(tc, args if isinstance(args, dict) else {})
    except Exception as e:  # surfaced to the model as an error result, never raised to the user
        return {"error": f"tool failed: {type(e).__name__}"}, True
    return result, "error" in result


def dumps(v: Any) -> str:
    return json.dumps(v, default=str, separators=(",", ":"))
