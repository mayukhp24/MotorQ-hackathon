"""Read-through cache with explicit invalidation (cache-aside pattern)."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Any

import orjson
from redis.asyncio import Redis


class Cache:
    def __init__(self, redis: Redis, prefix: str = "cache") -> None:
        self.r = redis
        self.prefix = prefix

    def key(self, tenant_id: str, name: str) -> str:
        return f"{self.prefix}:{tenant_id}:{name}"

    async def get_or_set(self, tenant_id: str, name: str, ttl_s: int,
                         loader: Callable[[], Awaitable[Any]]) -> Any:
        k = self.key(tenant_id, name)
        try:
            hit = await self.r.get(k)
        except Exception:  # cache outage degrades to direct reads
            hit = None
        if hit is not None:
            return orjson.loads(hit)
        value = await loader()
        try:
            await self.r.set(k, orjson.dumps(value, default=str), ex=ttl_s)
        except Exception:
            pass
        return value

    async def invalidate(self, tenant_id: str, *names: str) -> None:
        if names:
            await self.r.delete(*[self.key(tenant_id, n) for n in names])
