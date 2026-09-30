"""Request/response DTOs (validation at the boundary)."""

from __future__ import annotations

from datetime import date
from typing import Any, Literal

from pydantic import BaseModel, Field

VIN_PATTERN = r"^[A-HJ-NPR-Z0-9]{17}$"
UUID_PATTERN = r"^[0-9a-fA-F-]{36}$"


class TokenResponse(BaseModel):
    access_token: str
    token_type: str = "bearer"
    expires_in: int
    user: dict[str, Any]


class Page(BaseModel):
    items: list[dict[str, Any]]
    next_cursor: str | None = None


class WorkOrderCreate(BaseModel):
    vin: str = Field(pattern=VIN_PATTERN)
    component: Literal["cooling", "battery12v", "misfire", "ev_pack", "other"] | None = None
    priority: Literal["P1", "P2", "P3"] = "P2"
    due_on: date | None = None
    notes: str | None = Field(default=None, max_length=2000)
    source: Literal["PREDICTION", "ALERT", "MANUAL"] = "MANUAL"


class WorkOrderPatch(BaseModel):
    status: Literal["OPEN", "SCHEDULED", "IN_PROGRESS", "DONE", "CANCELLED"] | None = None
    notes: str | None = Field(default=None, max_length=2000)
    due_on: date | None = None


class ChatRequest(BaseModel):
    message: str = Field(min_length=1, max_length=4000)
    conversation_id: str | None = Field(default=None, pattern=UUID_PATTERN)


class ErasureRequest(BaseModel):
    driver_id: str = Field(pattern=UUID_PATTERN)
    reason: str = Field(min_length=3, max_length=500)
