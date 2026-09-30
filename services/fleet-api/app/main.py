"""FleetPulse Fleet API – composition root."""

from __future__ import annotations

import hashlib
import json
import logging
import sys
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException, Request
from fastapi.exceptions import RequestValidationError
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse, ORJSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from redis.asyncio import Redis

from app.agent.copilot import Copilot
from app.api import auth, compliance, copilot, fleet, ops
from app.api.middleware import ObservabilityMiddleware
from app.config import Settings, get_settings
from app.context import AppContext
from app.domain.pagination import CursorError
from app.infra import db
from app.infra.audit import AuditWriter
from app.infra.cache import Cache
from app.infra.clickhouse import ClickHouse
from app.security.jwt import TokenService, generate_private_key_pem
from app.security.ratelimit import RateLimiter


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        base = {"ts": self.formatTime(record), "level": record.levelname, "logger": record.name,
                "msg": record.getMessage(), "service": "fleet-api"}
        for k in ("request_id", "method", "route", "status", "duration_ms"):
            if hasattr(record, k):
                base[k] = getattr(record, k)
        if record.exc_info:
            base["exc"] = self.formatException(record.exc_info)
        return json.dumps(base)


def configure_logging() -> None:
    h = logging.StreamHandler(sys.stdout)
    h.setFormatter(JsonFormatter())
    logging.basicConfig(level=logging.INFO, handlers=[h], force=True)


async def signing_key(settings: Settings, redis: Redis) -> str:
    """Use the injected key; otherwise share one generated key across all
    workers/replicas through Redis (SET NX), so any replica can verify."""
    if settings.jwt_private_key_pem:
        return settings.jwt_private_key_pem
    candidate = generate_private_key_pem()
    await redis.set("jwt:signing_key", candidate, nx=True)
    return (await redis.get("jwt:signing_key")).decode()


async def build_context(settings: Settings) -> AppContext:
    redis = Redis.from_url(settings.redis_url)
    live = Redis.from_url(settings.live_redis_url, decode_responses=True)
    key = await signing_key(settings, redis)
    if not settings.cursor_secret:
        settings.cursor_secret = hashlib.sha256(("cursor|" + key).encode()).hexdigest()
    ctx = AppContext(
        settings=settings,
        tokens=TokenService(key, settings.jwt_issuer, settings.jwt_audience, settings.jwt_ttl_minutes),
        redis=redis, live=live,
        ch=ClickHouse(settings.clickhouse_url, settings.clickhouse_user, settings.clickhouse_password,
                      settings.clickhouse_timeout_s),
        cache=Cache(redis), limiter=RateLimiter(redis), audit=AuditWriter(),
    )
    ctx.copilot = Copilot(ctx)
    return ctx


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    configure_logging()
    settings = get_settings()
    await db.init_pool(settings.database_url)
    ctx = await build_context(settings)
    app.state.ctx = ctx
    ctx.audit.start()
    logging.getLogger("startup").info("fleet-api ready (copilot mode: %s)",
                                      "llm" if ctx.copilot and ctx.copilot.client else "offline")
    try:
        yield
    finally:
        await ctx.audit.stop()
        await ctx.ch.close()
        await ctx.redis.aclose()
        await ctx.live.aclose()
        await db.close_pool()


def problem(status: int, title: str, detail: str | None, request: Request, headers: dict | None = None) -> JSONResponse:
    """RFC 9457 problem details."""
    return JSONResponse(status_code=status, media_type="application/problem+json", headers=headers,
                        content={"type": f"https://fleetpulse.dev/problems/{status}", "title": title, "status": status,
                                 "detail": detail, "instance": request.url.path,
                                 "request_id": getattr(request.state, "request_id", None)})


def create_app() -> FastAPI:
    settings = get_settings()
    app = FastAPI(title="FleetPulse Fleet API", version="1.0.0", lifespan=lifespan,
                  default_response_class=ORJSONResponse,
                  description="Tenant-isolated fleet intelligence API: live fleet state, predictive maintenance, "
                              "alerts, analytics, compliance and an AI maintenance copilot. Auth: OAuth2 bearer JWT "
                              "(RS256). Pagination: opaque keyset cursors. Errors: RFC 9457 problem+json.")
    app.add_middleware(ObservabilityMiddleware)
    app.add_middleware(CORSMiddleware, allow_origins=settings.cors_list, allow_credentials=False,
                       allow_methods=["GET", "POST", "PATCH", "DELETE"],
                       allow_headers=["Authorization", "Content-Type", "If-Match", "X-Request-ID", "X-Tenant-Id"],
                       expose_headers=["X-Request-ID", "X-RateLimit-Remaining", "Retry-After"])

    @app.exception_handler(HTTPException)
    async def http_exc(request: Request, exc: HTTPException) -> JSONResponse:
        return problem(exc.status_code, _TITLES.get(exc.status_code, "Error"), str(exc.detail), request,
                       getattr(exc, "headers", None))

    @app.exception_handler(RequestValidationError)
    async def validation_exc(request: Request, exc: RequestValidationError) -> JSONResponse:
        errors = [{"loc": list(e["loc"]), "msg": e["msg"]} for e in exc.errors()]
        resp = problem(422, "Validation failed", "request did not match the schema", request)
        body = json.loads(resp.body)
        body["errors"] = errors
        return JSONResponse(status_code=422, media_type="application/problem+json", content=body)

    @app.exception_handler(CursorError)
    async def cursor_exc(request: Request, exc: CursorError) -> JSONResponse:
        return problem(400, "Invalid cursor", str(exc), request)

    @app.exception_handler(Exception)
    async def unhandled(request: Request, exc: Exception) -> JSONResponse:
        logging.getLogger("error").exception("unhandled error", extra={"request_id": getattr(request.state, "request_id", None)})
        return problem(500, "Internal error", "an unexpected error occurred", request)

    for r in (auth.router, fleet.router, ops.router, copilot.router, compliance.router):
        app.include_router(r)

    @app.get("/healthz", include_in_schema=False)
    async def healthz() -> dict:
        return {"status": "ok"}

    @app.get("/readyz", include_in_schema=False)
    async def readyz(request: Request) -> JSONResponse:
        ctx: AppContext = request.app.state.ctx
        try:
            async with db.pool().acquire() as conn:
                await conn.fetchval("SELECT 1")
            await ctx.redis.ping()
        except Exception as e:
            return JSONResponse({"status": "not ready", "error": type(e).__name__}, status_code=503)
        return JSONResponse({"status": "ready"})

    @app.get("/metrics", include_in_schema=False)
    async def metrics() -> PlainTextResponse:
        return PlainTextResponse(generate_latest().decode(), media_type=CONTENT_TYPE_LATEST)

    if settings.otel_exporter_otlp_endpoint:  # optional distributed tracing
        try:
            from app.main_otel import instrument

            instrument(app, settings)
        except ImportError:
            logging.getLogger("startup").warning("OpenTelemetry extras not installed")
    return app


_TITLES = {400: "Bad request", 401: "Unauthorized", 403: "Forbidden", 404: "Not found", 409: "Conflict",
           422: "Validation failed", 423: "Locked", 429: "Too many requests", 503: "Service unavailable"}

app = create_app()
