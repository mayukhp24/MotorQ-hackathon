"""Request-scoped dependencies: authentication, authorisation, rate limits."""

from __future__ import annotations

from collections.abc import Callable

import jwt
from fastapi import Depends, HTTPException, Request, status

from app.context import AppContext
from app.infra.audit import AuditEvent
from app.security import rbac
from app.security.jwt import Principal
from app.security.rbac import permissions_for


def get_ctx(request: Request) -> AppContext:
    return request.app.state.ctx


def client_ip(request: Request) -> str | None:
    return request.client.host if request.client else None


def _unauthorized(detail: str) -> HTTPException:
    return HTTPException(status.HTTP_401_UNAUTHORIZED, detail, headers={"WWW-Authenticate": "Bearer"})


def principal_from_token(ctx: AppContext, token: str, tenant_override: str | None = None) -> Principal:
    try:
        claims = ctx.tokens.verify(token)
    except jwt.ExpiredSignatureError as e:
        raise _unauthorized("token expired") from e
    except jwt.PyJWTError as e:
        raise _unauthorized("invalid token") from e
    roles = tuple(claims.get("roles") or [])
    perms = permissions_for(roles)
    tenant = claims.get("tid")
    if tenant_override and rbac.PLATFORM_ADMIN in perms:
        tenant = tenant_override  # operators act on a tenant explicitly; audited below
    return Principal(user_id=claims["sub"], tenant_id=tenant, email=claims.get("email", ""),
                     name=claims.get("name", ""), roles=roles, permissions=perms, token_id=claims.get("jti", ""))


async def current_principal(request: Request, ctx: AppContext = Depends(get_ctx)) -> Principal:
    return await _authenticate(request, ctx, need_tenant=True)


async def platform_principal(request: Request, ctx: AppContext = Depends(get_ctx)) -> Principal:
    """Platform-operator endpoints: cross-tenant aggregates, no tenant needed."""
    p = await _authenticate(request, ctx, need_tenant=False)
    if not p.can(rbac.PLATFORM_ADMIN):
        raise HTTPException(status.HTTP_403_FORBIDDEN, f"permission '{rbac.PLATFORM_ADMIN}' required")
    return p


async def _authenticate(request: Request, ctx: AppContext, need_tenant: bool) -> Principal:
    auth = request.headers.get("authorization", "")
    if not auth.lower().startswith("bearer "):
        raise _unauthorized("missing bearer token")
    p = principal_from_token(ctx, auth[7:].strip(), request.headers.get("x-tenant-id"))
    if await ctx.redis.exists(f"revoked:{p.token_id}"):
        raise _unauthorized("token revoked")
    if need_tenant and not p.tenant_id:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "no tenant selected (platform operators: send X-Tenant-Id)")
    # Per-user rate limit, shared across replicas.
    decision = await ctx.limiter.hit(f"user:{p.user_id}", ctx.settings.rate_limit_per_minute)
    request.state.ratelimit = decision
    if not decision.allowed:
        raise HTTPException(status.HTTP_429_TOO_MANY_REQUESTS, "rate limit exceeded",
                            headers={"Retry-After": str(decision.reset_s)})
    request.state.principal = p
    return p


def require(permission: str) -> Callable[..., Principal]:
    async def dep(request: Request, p: Principal = Depends(current_principal),
                  ctx: AppContext = Depends(get_ctx)) -> Principal:
        if not p.can(permission):
            ctx.audit.record(AuditEvent(action=f"{request.method} {request.url.path}", resource_type="authz",
                                        outcome="DENY", tenant_id=p.tenant_id, actor_id=p.user_id,
                                        ip=client_ip(request), request_id=getattr(request.state, "request_id", None),
                                        details={"missing_permission": permission}))
            raise HTTPException(status.HTTP_403_FORBIDDEN, f"permission '{permission}' required")
        return p

    return dep
