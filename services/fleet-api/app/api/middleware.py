"""Cross-cutting HTTP concerns: request IDs, structured access logs,
Prometheus RED metrics, security headers and the data-access audit trail."""

from __future__ import annotations

import logging
import time
import uuid

from prometheus_client import Counter, Histogram
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response

from app.infra.audit import AuditEvent

log = logging.getLogger("access")

REQ_LAT = Histogram("api_request_duration_seconds", "HTTP request latency", ["method", "route", "status"],
                    buckets=[0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5, 10])
REQ_TOTAL = Counter("api_requests_total", "HTTP requests", ["method", "route", "status"])

SECURITY_HEADERS = {
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
    "Cross-Origin-Opener-Policy": "same-origin",
    "Permissions-Policy": "geolocation=(), camera=(), microphone=()",
    "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
    "Cache-Control": "no-store",
}


def _route_template(request: Request) -> str:
    route = request.scope.get("route")
    return getattr(route, "path", None) or "unmatched"


class ObservabilityMiddleware(BaseHTTPMiddleware):
    async def dispatch(self, request: Request, call_next) -> Response:  # type: ignore[override]
        rid = request.headers.get("x-request-id") or uuid.uuid4().hex
        request.state.request_id = rid
        start = time.perf_counter()
        status = 500
        try:
            response = await call_next(request)
            status = response.status_code
        finally:
            elapsed = time.perf_counter() - start
            route = _route_template(request)
            REQ_LAT.labels(request.method, route, str(status)).observe(elapsed)
            REQ_TOTAL.labels(request.method, route, str(status)).inc()
            if route not in ("/metrics", "/healthz", "/readyz"):
                log.info("request", extra={"request_id": rid, "method": request.method, "route": route,
                                           "status": status, "duration_ms": round(elapsed * 1000, 1)})
        response.headers["X-Request-ID"] = rid
        for k, v in SECURITY_HEADERS.items():
            response.headers.setdefault(k, v)
        if request.url.path.startswith("/docs") or request.url.path.startswith("/redoc"):
            del response.headers["Content-Security-Policy"]
        rl = getattr(request.state, "ratelimit", None)
        if rl is not None:
            response.headers["X-RateLimit-Limit"] = str(rl.limit)
            response.headers["X-RateLimit-Remaining"] = str(rl.remaining)
            response.headers["X-RateLimit-Reset"] = str(rl.reset_s)
        self._audit(request, route, status)
        return response

    @staticmethod
    def _audit(request: Request, route: str, status: int) -> None:
        """Every authenticated data access is audited (who, what, when, outcome)."""
        p = getattr(request.state, "principal", None)
        if p is None or not route.startswith("/api/"):
            return
        outcome = "ALLOW" if status < 400 else ("DENY" if status in (401, 403, 429) else "ERROR")
        params = request.path_params
        resource_id = next((str(params[k]) for k in ("vin", "alert_id", "work_order_id", "action_id", "driver_id")
                            if k in params), None)
        ctx = request.app.state.ctx
        ctx.audit.record(AuditEvent(
            action=f"{request.method} {route}", resource_type=route.split("/")[3] if route.count("/") >= 3 else route,
            outcome=outcome, tenant_id=p.tenant_id, actor_id=p.user_id, resource_id=resource_id,
            ip=request.client.host if request.client else None, request_id=request.state.request_id,
            details={"query": dict(request.query_params), "status": status}))
