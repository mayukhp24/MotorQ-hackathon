"""Asynchronous, batched audit trail.

Every authenticated data access and every copilot action is recorded. Writes
are queued in-process and flushed in small batches so auditing never adds a
database round-trip to request latency; the database trigger hash-chains
each row (tamper evidence). On shutdown the queue is drained.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field
from typing import Any

from prometheus_client import Counter

from app.infra import db

log = logging.getLogger("audit")
AUDIT_DROPPED = Counter("api_audit_dropped_total", "Audit events dropped (queue full)")
AUDIT_WRITTEN = Counter("api_audit_written_total", "Audit events persisted")

_SQL = """INSERT INTO audit_log (tenant_id, actor_id, actor_type, action, resource_type, resource_id, outcome, ip,
            request_id, details, hash)
          VALUES ($1, $2, $3, $4, $5, $6, $7, $8::inet, $9, $10, '\\x00')"""


@dataclass
class AuditEvent:
    action: str
    resource_type: str
    outcome: str = "ALLOW"
    tenant_id: str | None = None
    actor_id: str | None = None
    actor_type: str = "USER"
    resource_id: str | None = None
    ip: str | None = None
    request_id: str | None = None
    details: dict[str, Any] = field(default_factory=dict)


class AuditWriter:
    def __init__(self, max_queue: int = 50_000, batch: int = 500, interval_s: float = 0.2) -> None:
        self.q: asyncio.Queue[AuditEvent] = asyncio.Queue(maxsize=max_queue)
        self.batch = batch
        self.interval = interval_s
        self._task: asyncio.Task | None = None

    def record(self, ev: AuditEvent) -> None:
        try:
            self.q.put_nowait(ev)
        except asyncio.QueueFull:
            AUDIT_DROPPED.inc()
            log.error("audit queue full; event dropped", extra={"action": ev.action})

    def start(self) -> None:
        self._task = asyncio.create_task(self._run())

    async def stop(self) -> None:
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        await self.flush()

    async def _run(self) -> None:
        while True:
            await asyncio.sleep(self.interval)
            try:
                await self.flush()
            except Exception:  # keep the writer alive; events stay queued
                log.exception("audit flush failed")

    async def flush(self) -> int:
        items: list[AuditEvent] = []
        while not self.q.empty() and len(items) < self.batch:
            items.append(self.q.get_nowait())
        if not items:
            return 0
        # Group by tenant: RLS WITH CHECK requires app.tenant_id to match.
        by_tenant: dict[str | None, list[AuditEvent]] = {}
        for ev in items:
            by_tenant.setdefault(ev.tenant_id, []).append(ev)
        for tenant, evs in by_tenant.items():
            async with db.tenant_tx(tenant, readonly=False) as conn:
                await conn.executemany(_SQL, [
                    (e.tenant_id, e.actor_id, e.actor_type, e.action, e.resource_type, e.resource_id, e.outcome,
                     e.ip, e.request_id, e.details) for e in evs])
        AUDIT_WRITTEN.inc(len(items))
        return len(items)
