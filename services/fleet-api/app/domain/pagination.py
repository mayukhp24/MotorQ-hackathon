"""Opaque, tamper-evident keyset cursors.

Keyset pagination (WHERE (k1, k2) < ($1, $2) ORDER BY k1, k2 LIMIT n) is
O(log n + page) at any depth, unlike OFFSET which scans and discards every
skipped row. Cursors are HMAC-signed so clients cannot forge positions.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
from typing import Any

import orjson


class CursorError(ValueError):
    pass


def encode_cursor(values: dict[str, Any], secret: str) -> str:
    body = orjson.dumps(values, default=str)
    sig = hmac.new(secret.encode(), body, hashlib.sha256).digest()[:12]
    return base64.urlsafe_b64encode(sig + body).rstrip(b"=").decode()


def decode_cursor(cursor: str | None, secret: str) -> dict[str, Any] | None:
    if not cursor:
        return None
    try:
        raw = base64.urlsafe_b64decode(cursor + "=" * (-len(cursor) % 4))
    except (ValueError, TypeError) as e:
        raise CursorError("malformed cursor") from e
    sig, body = raw[:12], raw[12:]
    if not hmac.compare_digest(sig, hmac.new(secret.encode(), body, hashlib.sha256).digest()[:12]):
        raise CursorError("invalid cursor signature")
    try:
        value = orjson.loads(body)
    except orjson.JSONDecodeError as e:
        raise CursorError("malformed cursor") from e
    if not isinstance(value, dict):
        raise CursorError("malformed cursor")
    return value


def clamp_limit(limit: int | None, default: int = 50, maximum: int = 200) -> int:
    if limit is None:
        return default
    return max(1, min(maximum, int(limit)))
