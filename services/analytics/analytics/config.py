"""Environment configuration for batch jobs (12-factor)."""

import os
from dataclasses import dataclass, field


def _env(key: str, default: str) -> str:
    return os.environ.get(key) or default


@dataclass(frozen=True)
class Config:
    database_url: str = field(default_factory=lambda: _env(
        "DATABASE_URL", "postgresql://fleetpulse_writer:writer-local-only@localhost:5432/fleetpulse"))
    clickhouse_url: str = field(default_factory=lambda: _env("CLICKHOUSE_URL", "http://localhost:8123"))
    clickhouse_user: str = field(default_factory=lambda: _env("CLICKHOUSE_USER", "default"))
    clickhouse_password: str = field(default_factory=lambda: _env("CLICKHOUSE_PASSWORD", "clickhouse-local-only"))
    artifacts_dir: str = field(default_factory=lambda: _env("ARTIFACTS_DIR", "artifacts"))
    schedule: str = field(default_factory=lambda: _env("SCHEDULE", "daily"))
    score_backfill_days: int = field(default_factory=lambda: int(_env("SCORE_BACKFILL_DAYS", "7")))
    negative_sample_rate: float = field(default_factory=lambda: float(_env("NEGATIVE_SAMPLE_RATE", "0.25")))
    metrics_port: int = field(default_factory=lambda: int(_env("METRICS_PORT", "9105")))
