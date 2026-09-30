import { describe, expect, it, vi } from "vitest";
import { fmtAgo, fmtCompact, fmtInt, fmtPct, fmtUsd, titleCase } from "../lib/format";

describe("formatters", () => {
  it("renders missing values as an en dash", () => {
    for (const f of [fmtInt, fmtCompact, fmtUsd, fmtPct, fmtAgo]) expect(f(null)).toBe("–");
    expect(fmtInt(Number.NaN)).toBe("–");
  });

  it("formats numbers, currency and percentages", () => {
    expect(fmtInt(100000.4)).toBe("100,000");
    expect(fmtCompact(125_400)).toBe("125.4K");
    expect(fmtUsd(2400)).toBe("$2,400");
    expect(fmtUsd(1_250_000, true)).toBe("$1.3M");
    expect(fmtPct(0.1234, 1)).toBe("12.3%");
  });

  it("describes elapsed time relative to now", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-30T12:00:00Z"));
    expect(fmtAgo(Date.parse("2026-09-30T11:59:30Z"))).toBe("30s ago");
    expect(fmtAgo("2026-09-30T11:15:00Z")).toBe("45m ago");
    expect(fmtAgo("2026-09-30T06:00:00Z")).toBe("6h ago");
    expect(fmtAgo("2026-09-27T12:00:00Z")).toBe("3d ago");
    vi.useRealTimers();
  });

  it("title-cases enum values", () => {
    expect(titleCase("HARSH_BRAKING")).toBe("Harsh Braking");
    expect(titleCase("dtc")).toBe("Dtc");
  });
});
