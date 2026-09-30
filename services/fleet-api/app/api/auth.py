"""OAuth2 password grant (local IdP), token introspection helpers, JWKS."""

from __future__ import annotations

import asyncio

import bcrypt
from fastapi import APIRouter, Depends, Form, HTTPException, Request, Response, status

from app.api.deps import client_ip, current_principal, get_ctx
from app.api.schemas import TokenResponse
from app.context import AppContext
from app.infra import db
from app.infra.audit import AuditEvent
from app.security.jwt import Principal
from app.security.rbac import permissions_for

router = APIRouter(tags=["auth"])

# Constant-time dummy hash so unknown users cost the same as wrong passwords.
_DUMMY_HASH = bcrypt.hashpw(b"not-a-real-password", bcrypt.gensalt(10))


@router.post("/api/v1/auth/token", response_model=TokenResponse, summary="OAuth2 password grant")
async def token(request: Request, username: str = Form(...), password: str = Form(...),
                grant_type: str = Form("password"), ctx: AppContext = Depends(get_ctx)) -> TokenResponse:
    if grant_type != "password":
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "unsupported_grant_type")
    ip = client_ip(request) or "unknown"
    email = username.strip().lower()
    decision = await ctx.limiter.hit(f"login:{ip}", ctx.settings.login_rate_limit_per_minute)
    if not decision.allowed:
        raise HTTPException(status.HTTP_429_TOO_MANY_REQUESTS, "too many login attempts",
                            headers={"Retry-After": str(decision.reset_s)})
    lock_key = f"lockout:{email}"
    if int(await ctx.redis.get(lock_key) or 0) >= ctx.settings.login_max_failures:
        raise HTTPException(status.HTTP_423_LOCKED, "account temporarily locked after repeated failures")

    async with db.tenant_tx(None) as conn:
        user = await conn.fetchrow("SELECT * FROM auth_lookup($1)", email)
    hashed = user["password_hash"].encode() if user else _DUMMY_HASH
    ok = await asyncio.to_thread(bcrypt.checkpw, password.encode(), hashed)
    if not user or not ok or user["status"] != "ACTIVE":
        fails = await ctx.redis.incr(lock_key)
        await ctx.redis.expire(lock_key, ctx.settings.login_lockout_minutes * 60)
        ctx.audit.record(AuditEvent(action="auth.login", resource_type="auth", outcome="DENY",
                                    tenant_id=str(user["tenant_id"]) if user and user["tenant_id"] else None,
                                    actor_id=str(user["user_id"]) if user else None, ip=ip,
                                    details={"email": email, "failures": fails}))
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "invalid credentials")
    await ctx.redis.delete(lock_key)
    tenant = str(user["tenant_id"]) if user["tenant_id"] else None
    roles = list(user["roles"])
    tok, ttl = ctx.tokens.issue(user_id=str(user["user_id"]), tenant_id=tenant, email=email,
                                name=user["full_name"], roles=roles)
    async with db.tenant_tx(tenant, readonly=False) as conn:
        await conn.execute("UPDATE app_user SET last_login_at = now() WHERE user_id = $1", user["user_id"])
    ctx.audit.record(AuditEvent(action="auth.login", resource_type="auth", tenant_id=tenant,
                                actor_id=str(user["user_id"]), ip=ip, details={"roles": roles}))
    return TokenResponse(access_token=tok, expires_in=ttl, user={
        "user_id": str(user["user_id"]), "email": email, "name": user["full_name"], "tenant_id": tenant,
        "roles": roles, "permissions": sorted(permissions_for(roles))})


@router.post("/api/v1/auth/logout", status_code=204, response_class=Response, summary="Revoke the current token")
async def logout(request: Request, p: Principal = Depends(current_principal), ctx: AppContext = Depends(get_ctx)) -> Response:
    await ctx.redis.set(f"revoked:{p.token_id}", 1, ex=ctx.settings.jwt_ttl_minutes * 60)
    return Response(status_code=204)


@router.get("/api/v1/auth/me", summary="Current principal")
async def me(p: Principal = Depends(current_principal), ctx: AppContext = Depends(get_ctx)) -> dict:
    tenant_name = None
    async with db.tenant_tx(p.tenant_id) as conn:
        tenant_name = await conn.fetchval("SELECT name FROM tenant WHERE tenant_id = $1::uuid", p.tenant_id)
    return {"user_id": p.user_id, "email": p.email, "name": p.name, "tenant_id": p.tenant_id,
            "tenant_name": tenant_name, "roles": list(p.roles), "permissions": sorted(p.permissions)}


@router.get("/.well-known/jwks.json", include_in_schema=False)
async def jwks(ctx: AppContext = Depends(get_ctx)) -> dict:
    return ctx.tokens.jwks()


@router.get("/.well-known/openid-configuration", include_in_schema=False)
async def oidc(ctx: AppContext = Depends(get_ctx)) -> dict:
    iss = ctx.settings.jwt_issuer
    return {"issuer": iss, "jwks_uri": "/.well-known/jwks.json", "token_endpoint": "/api/v1/auth/token",
            "grant_types_supported": ["password"], "id_token_signing_alg_values_supported": ["RS256"]}
