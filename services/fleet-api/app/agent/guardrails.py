"""Copilot guardrails: input screening, output grounding and tool-argument
validation. Defence in depth – the real protection is that tools are
read-only or proposal-only, tenant-scoped server-side and permission-checked,
so even a successful injection cannot exfiltrate other tenants' data or act
without a human approval."""

from __future__ import annotations

import re
from dataclasses import dataclass

_INJECTION = [
    r"ignore (all |any )?(previous|prior|above) (instructions|rules)",
    r"disregard (the )?(system|previous) (prompt|instructions)",
    r"(reveal|print|show|repeat) (your|the) (system prompt|instructions|hidden)",
    r"you are now (?!looking)",
    r"act as (an? )?(admin|administrator|root|developer|dan)",
    r"\bjailbreak\b",
    r"tenant[_ ]?id\s*[:=]",
    r"</?(system|tool_result|function_calls)>",
    r"set_config\(|app\.tenant_id|pg_sleep|;\s*drop\s+table",
]
_INJECTION_RE = [re.compile(p, re.IGNORECASE) for p in _INJECTION]
VIN_RE = re.compile(r"\b[A-HJ-NPR-Z0-9]{17}\b")


@dataclass
class Screen:
    allowed: bool
    reason: str | None = None


def screen_input(text: str, max_chars: int) -> Screen:
    if not text or not text.strip():
        return Screen(False, "empty message")
    if len(text) > max_chars:
        return Screen(False, f"message longer than {max_chars} characters")
    for rx in _INJECTION_RE:
        if rx.search(text):
            return Screen(False, "request looks like an attempt to override the assistant's instructions")
    return Screen(True)


def ungrounded_vins(answer: str, evidence: str) -> list[str]:
    """VINs the model mentions that never appeared in tool results or the
    user's question – a cheap hallucination check for identifiers."""
    seen = set(VIN_RE.findall(evidence))
    return sorted({v for v in VIN_RE.findall(answer) if v not in seen})


def clamp_int(v: object, lo: int, hi: int, default: int) -> int:
    try:
        return max(lo, min(hi, int(v)))  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return default


def clamp_float(v: object, lo: float, hi: float, default: float) -> float:
    try:
        return max(lo, min(hi, float(v)))  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return default
