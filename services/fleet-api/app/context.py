"""Process-wide dependencies, created once at start-up (composition root)."""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING

from redis.asyncio import Redis

from app.config import Settings
from app.infra.audit import AuditWriter
from app.infra.cache import Cache
from app.infra.clickhouse import ClickHouse
from app.security.jwt import TokenService
from app.security.ratelimit import RateLimiter

if TYPE_CHECKING:
    from app.agent.copilot import Copilot


@dataclass
class AppContext:
    settings: Settings
    tokens: TokenService
    redis: Redis        # API state: cache, rate limits, tickets, conversations
    live: Redis         # read model written by the stream processor
    ch: ClickHouse
    cache: Cache
    limiter: RateLimiter
    audit: AuditWriter
    copilot: "Copilot | None" = None
