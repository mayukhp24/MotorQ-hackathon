"""PostgreSQL access (asyncpg). Every tenant-scoped query runs inside a
transaction that sets `app.tenant_id`, so row-level security in the database
enforces isolation even if application code forgets a WHERE clause."""

from __future__ import annotations

import json
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from decimal import Decimal
from typing import Any

import asyncpg

_pool: asyncpg.Pool | None = None


async def _init_conn(conn: asyncpg.Connection) -> None:
    await conn.set_type_codec("jsonb", encoder=json.dumps, decoder=json.loads, schema="pg_catalog")
    await conn.set_type_codec("json", encoder=json.dumps, decoder=json.loads, schema="pg_catalog")


async def init_pool(dsn: str, min_size: int = 2, max_size: int = 20) -> asyncpg.Pool:
    global _pool
    _pool = await asyncpg.create_pool(dsn, min_size=min_size, max_size=max_size, init=_init_conn,
                                      command_timeout=15, max_inactive_connection_lifetime=300)
    return _pool


async def close_pool() -> None:
    global _pool
    if _pool is not None:
        await _pool.close()
        _pool = None


def pool() -> asyncpg.Pool:
    if _pool is None:
        raise RuntimeError("database pool not initialised")
    return _pool


@asynccontextmanager
async def tenant_tx(tenant_id: str | None, *, readonly: bool = True) -> AsyncIterator[asyncpg.Connection]:
    """Yield a connection in a transaction bound to one tenant."""
    async with pool().acquire() as conn:
        async with conn.transaction(readonly=readonly):
            await conn.execute("SELECT set_config('app.tenant_id', $1, true)", tenant_id or "")
            yield conn


def plain(v: Any) -> Any:
    """Normalise driver types for JSON/caching: Decimal→float, UUID→str."""
    if isinstance(v, Decimal):
        return float(v)
    if isinstance(v, uuid.UUID):
        return str(v)
    return v


def row(record: asyncpg.Record | None) -> dict[str, Any] | None:
    return None if record is None else {k: plain(v) for k, v in record.items()}


def rows(records: list[asyncpg.Record]) -> list[dict[str, Any]]:
    return [{k: plain(v) for k, v in r.items()} for r in records]
