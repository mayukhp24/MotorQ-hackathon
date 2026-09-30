export const fmtInt = (n: number | null | undefined) => (n == null || Number.isNaN(n) ? "–" : Math.round(n).toLocaleString("en-US"));

export function fmtCompact(n: number | null | undefined) {
  if (n == null || Number.isNaN(n)) return "–";
  return Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(n);
}

export function fmtUsd(n: number | null | undefined, compact = false) {
  if (n == null || Number.isNaN(n)) return "–";
  return Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    maximumFractionDigits: compact ? 1 : 0,
    notation: compact ? "compact" : "standard",
  }).format(n);
}

export const fmtPct = (p: number | null | undefined, digits = 0) => (p == null ? "–" : `${(p * 100).toFixed(digits)}%`);

export function fmtAgo(ts: string | number | null | undefined) {
  if (!ts) return "–";
  const t = typeof ts === "number" ? ts : Date.parse(ts);
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

export function fmtDateTime(ts: string | number | null | undefined) {
  if (!ts) return "–";
  return new Date(ts).toLocaleString("en-GB", { day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit" });
}

export const fmtTime = (ts: number) => new Date(ts).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });

export const COMPONENT_LABEL: Record<string, string> = {
  cooling: "Cooling system",
  battery12v: "12 V battery / charging",
  misfire: "Ignition / misfire",
  ev_pack: "HV battery pack",
  other: "Other",
};

export const titleCase = (s: string) => s.toLowerCase().replace(/(^|_)(\w)/g, (_, sp, c) => (sp ? " " : "") + c.toUpperCase());
