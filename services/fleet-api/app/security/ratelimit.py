"""Distributed rate limiting (sliding window over two fixed buckets) in
Redis. Shared across API replicas; one Lua round-trip per request."""

from __future__ import annotations

import time
from dataclasses import dataclass

from redis.asyncio import Redis

# KEYS[1]=current bucket, KEYS[2]=previous bucket; ARGV: limit, window_s, elapsed_fraction
_LUA = """
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local prev = tonumber(redis.call('GET', KEYS[2]) or '0')
local limit = tonumber(ARGV[1])
local weighted = prev * (1 - tonumber(ARGV[3])) + cur
if weighted >= limit then
  return {0, math.floor(weighted)}
end
cur = redis.call('INCR', KEYS[1])
if cur == 1 then redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]) * 2) end
return {1, math.floor(weighted) + 1}
"""


@dataclass
class Decision:
    allowed: bool
    limit: int
    remaining: int
    reset_s: int


class RateLimiter:
    def __init__(self, redis: Redis, window_s: int = 60) -> None:
        self.r = redis
        self.window = window_s
        self._script = self.r.register_script(_LUA)

    async def hit(self, identity: str, limit: int, now: float | None = None) -> Decision:
        now = now or time.time()
        bucket = int(now // self.window)
        elapsed = (now % self.window) / self.window
        try:
            allowed, used = await self._script(
                keys=[f"rl:{identity}:{bucket}", f"rl:{identity}:{bucket - 1}"], args=[limit, self.window, elapsed])
        except Exception:
            # Fail open on limiter outage (availability over strictness), but
            # never for the login endpoint: the caller decides via `limit`.
            return Decision(True, limit, limit, self.window)
        reset = int(self.window - (now % self.window))
        return Decision(bool(allowed), limit, max(0, limit - int(used)), reset)
