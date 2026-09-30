"""12-factor configuration: everything comes from the environment."""

from functools import lru_cache

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=None, extra="ignore")

    service_name: str = "fleet-api"
    environment: str = "local"
    database_url: str = "postgresql://fleetpulse_api:api-local-only@localhost:5432/fleetpulse"
    redis_url: str = "redis://:redis-local-only@localhost:6379/1"
    live_redis_url: str = "redis://:redis-local-only@localhost:6379/0"
    clickhouse_url: str = "http://localhost:8123"
    clickhouse_user: str = "fleetpulse_ro"
    clickhouse_password: str = "clickhouse-ro-local-only"
    clickhouse_timeout_s: float = 8.0

    # Identity: RS256 JWTs. In production the private key is injected from the
    # secret store (or an external OIDC provider issues tokens and only
    # JWKS_URL is configured); locally a key is generated and shared via Redis.
    jwt_issuer: str = "https://fleetpulse.local/auth"
    jwt_audience: str = "fleetpulse-api"
    jwt_private_key_pem: str = ""
    jwt_ttl_minutes: int = 60
    login_max_failures: int = 5
    login_lockout_minutes: int = 15

    pii_encryption_key: str = Field(default="local-dev-pii-key-change-me", repr=False)
    cursor_secret: str = Field(default="", repr=False)

    rate_limit_per_minute: int = 1200
    login_rate_limit_per_minute: int = 20
    cors_origins: str = "http://localhost:3000,http://localhost:5173"

    # Copilot
    anthropic_api_key: str = Field(default="", repr=False)
    llm_model: str = "claude-opus-5-5"
    llm_effort: str = "medium"
    llm_max_tool_calls: int = 6
    llm_timeout_s: float = 60.0
    copilot_max_input_chars: int = 2000

    telemetry_partitions: int = 12
    snapshot_stale_s: int = 30
    otel_exporter_otlp_endpoint: str = ""

    @property
    def cors_list(self) -> list[str]:
        return [o.strip() for o in self.cors_origins.split(",") if o.strip()]


@lru_cache
def get_settings() -> Settings:
    return Settings()
