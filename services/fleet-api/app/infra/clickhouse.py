"""ClickHouse HTTP client for analytical reads.

The API connects as a read-only user whose row policy requires the custom
setting `SQL_tenant_id`; we always send it, so ClickHouse itself refuses to
return another tenant's telemetry. Parameters are bound server-side
(`{name:Type}` placeholders), never string-formatted.
"""

from __future__ import annotations

from typing import Any

import httpx
import orjson

from app.infra.breaker import CircuitBreaker


class ClickHouse:
    def __init__(self, url: str, user: str, password: str, timeout_s: float = 8.0) -> None:
        self._client = httpx.AsyncClient(base_url=url, auth=(user, password), timeout=timeout_s)
        self.breaker = CircuitBreaker("clickhouse", failure_threshold=5, reset_timeout_s=20)

    async def close(self) -> None:
        await self._client.aclose()

    async def query(self, sql: str, tenant_id: str, params: dict[str, Any] | None = None) -> list[dict[str, Any]]:
        q: dict[str, Any] = {"SQL_tenant_id": tenant_id, "default_format": "JSONEachRow",
                             # aggregate aliases may reuse column names (sum(events) AS events)
                             "prefer_column_name_to_alias": 1, "output_format_json_quote_64bit_integers": 0}
        for k, v in (params or {}).items():
            q[f"param_{k}"] = v

        async def _do() -> list[dict[str, Any]]:
            try:
                r = await self._client.post("/", params=q, content=sql.encode())
            except httpx.HTTPError as e:  # network failures count towards the breaker
                raise RuntimeError(f"clickhouse unreachable: {type(e).__name__}") from e
            if r.status_code != 200:
                raise RuntimeError(f"clickhouse {r.status_code}: {r.text[:300]}")
            return [orjson.loads(line) for line in r.content.splitlines() if line.strip()]

        return await self.breaker.call(_do)

    async def ping(self) -> bool:
        try:
            r = await self._client.get("/ping")
            return r.status_code == 200
        except httpx.HTTPError:
            return False
