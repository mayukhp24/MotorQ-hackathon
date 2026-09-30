"""Tiny helper that builds WHERE clauses with only the filters actually
supplied, keeping every value as a bound parameter ($n). Omitting unused
predicates (instead of `($1 IS NULL OR col = $1)`) lets the planner pick
partial and composite indexes for each concrete filter combination."""

from __future__ import annotations

from typing import Any


class Where:
    def __init__(self) -> None:
        self.clauses: list[str] = []
        self.args: list[Any] = []

    def param(self, value: Any) -> str:
        self.args.append(value)
        return f"${len(self.args)}"

    def add(self, template: str, *values: Any) -> "Where":
        """template uses {} placeholders, one per value."""
        self.clauses.append(template.format(*[self.param(v) for v in values]))
        return self

    def add_if(self, cond: bool, template: str, *values: Any) -> "Where":
        if cond:
            self.add(template, *values)
        return self

    def sql(self) -> str:
        return ("WHERE " + " AND ".join(self.clauses)) if self.clauses else ""
