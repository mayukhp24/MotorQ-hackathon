"""Minimal async circuit breaker (closed → open → half-open).

Wraps calls to slow or failing dependencies (ClickHouse, the LLM API) so the
API fails fast and degrades gracefully instead of piling up requests.
"""

from __future__ import annotations

import time
from collections.abc import Awaitable, Callable
from typing import TypeVar

from prometheus_client import Gauge

T = TypeVar("T")

BREAKER_STATE = Gauge("api_circuit_breaker_open", "1 when the breaker is open", ["name"])


class CircuitOpenError(RuntimeError):
    """Raised when a call is short-circuited."""


class CircuitBreaker:
    def __init__(self, name: str, failure_threshold: int = 5, reset_timeout_s: float = 30.0,
                 clock: Callable[[], float] = time.monotonic) -> None:
        self.name = name
        self.failure_threshold = failure_threshold
        self.reset_timeout_s = reset_timeout_s
        self._clock = clock
        self._failures = 0
        self._opened_at: float | None = None
        self._half_open_trial = False

    @property
    def state(self) -> str:
        if self._opened_at is None:
            return "closed"
        if self._clock() - self._opened_at >= self.reset_timeout_s:
            return "half_open"
        return "open"

    async def call(self, fn: Callable[[], Awaitable[T]]) -> T:
        state = self.state
        if state == "open" or (state == "half_open" and self._half_open_trial):
            raise CircuitOpenError(f"{self.name} circuit open")
        if state == "half_open":
            self._half_open_trial = True
        try:
            result = await fn()
        except Exception:
            self._on_failure()
            raise
        self._on_success()
        return result

    def _on_failure(self) -> None:
        self._failures += 1
        self._half_open_trial = False
        if self._opened_at is not None or self._failures >= self.failure_threshold:
            self._opened_at = self._clock()
            BREAKER_STATE.labels(self.name).set(1)

    def _on_success(self) -> None:
        self._failures = 0
        self._opened_at = None
        self._half_open_trial = False
        BREAKER_STATE.labels(self.name).set(0)
