import clsx from "clsx";
import { AlertOctagon, AlertTriangle, CircleDot, Info, Loader2, type LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

export function Card({ title, subtitle, action, children, className, pad = true }: {
  title?: ReactNode; subtitle?: ReactNode; action?: ReactNode; children: ReactNode; className?: string; pad?: boolean;
}) {
  return (
    <section className={clsx("card", className)}>
      {(title || action) && (
        <header className="flex items-start justify-between gap-3 px-4 pt-4 sm:px-5">
          <div className="min-w-0">
            {title && <h2 className="text-sm font-semibold text-ink-1">{title}</h2>}
            {subtitle && <p className="mt-0.5 text-xs text-ink-3">{subtitle}</p>}
          </div>
          {action && <div className="shrink-0">{action}</div>}
        </header>
      )}
      <div className={clsx(pad && "card-pad", title && pad && "pt-3")}>{children}</div>
    </section>
  );
}

export function PageHeader({ title, subtitle, actions }: { title: string; subtitle?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="mb-5 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 className="text-xl font-semibold tracking-tight text-ink-1">{title}</h1>
        {subtitle && <p className="mt-1 text-sm text-ink-2">{subtitle}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

/** Stat tile: the number is the chart. Text wears ink tokens; colour only as an accent bar. */
export function Kpi({ label, value, hint, icon: Icon, tone }: {
  label: string; value: ReactNode; hint?: ReactNode; icon?: LucideIcon; tone?: "critical" | "serious" | "warning" | "good" | "accent";
}) {
  return (
    <div className="card relative overflow-hidden p-4">
      {tone && (
        <span className="absolute inset-y-0 left-0 w-1" style={{ background: tone === "accent" ? "var(--accent)" : `var(--status-${tone})` }} />
      )}
      <div className="flex items-center justify-between">
        <span className="label">{label}</span>
        {Icon && <Icon className="h-4 w-4 text-ink-3" aria-hidden />}
      </div>
      <div className="mt-2 text-2xl font-semibold tabular-nums tracking-tight text-ink-1">{value}</div>
      {hint && <div className="mt-1 text-xs text-ink-3">{hint}</div>}
    </div>
  );
}

const SEV: Record<string, { icon: LucideIcon; color: string; label: string }> = {
  CRITICAL: { icon: AlertOctagon, color: "var(--status-critical)", label: "Critical" },
  HIGH: { icon: AlertTriangle, color: "var(--status-serious)", label: "High" },
  MEDIUM: { icon: AlertTriangle, color: "var(--status-warning)", label: "Medium" },
  LOW: { icon: Info, color: "var(--neutral-mark)", label: "Low" },
};

/** Severity always ships icon + label (never colour alone). */
export function SeverityBadge({ severity }: { severity: string }) {
  const s = SEV[severity] ?? SEV.LOW;
  const Icon = s.icon;
  return (
    <span className="inline-flex items-center gap-1 rounded-md border border-line bg-surface-2 px-1.5 py-0.5 text-xs font-medium text-ink-1">
      <Icon className="h-3.5 w-3.5" style={{ color: s.color }} aria-hidden />
      {s.label}
    </span>
  );
}

const VSTATUS: Record<string, { color: string; label: string }> = {
  DRIVING: { color: "var(--status-good)", label: "Driving" },
  IDLING: { color: "var(--status-warning)", label: "Idling" },
  CHARGING: { color: "var(--accent)", label: "Charging" },
  PARKED: { color: "var(--neutral-mark)", label: "Parked" },
  OFFLINE: { color: "var(--text-muted)", label: "Offline" },
};

export function VehicleStatus({ status, critical }: { status?: string | null; critical?: boolean }) {
  const s = VSTATUS[status || "OFFLINE"] ?? VSTATUS.OFFLINE;
  return (
    <span className="inline-flex items-center gap-1.5 text-sm text-ink-1">
      <CircleDot className="h-3.5 w-3.5" style={{ color: s.color }} aria-hidden />
      {s.label}
      {critical && <AlertOctagon className="h-3.5 w-3.5" style={{ color: "var(--status-critical)" }} aria-label="active critical fault" />}
    </span>
  );
}

export function RiskBar({ risk }: { risk: number | null | undefined }) {
  if (risk == null) return <span className="text-ink-3">–</span>;
  const tone = risk >= 0.5 ? "critical" : risk >= 0.25 ? "serious" : risk >= 0.1 ? "warning" : "good";
  return (
    <div className="flex items-center gap-2" title={`${(risk * 100).toFixed(1)}% probability of breakdown within 7 days`}>
      <div className="h-1.5 w-16 overflow-hidden rounded-full bg-surface-2">
        <div className="h-full rounded-full" style={{ width: `${Math.max(3, risk * 100)}%`, background: `var(--status-${tone})` }} />
      </div>
      <span className="w-10 text-right text-sm tabular-nums text-ink-1">{(risk * 100).toFixed(0)}%</span>
    </div>
  );
}

export function Spinner({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 p-8 text-sm text-ink-3">
      <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> {label}…
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="p-8 text-center text-sm text-ink-3">{children}</div>;
}

export function ErrorNote({ error }: { error: unknown }) {
  const msg = error instanceof Error ? error.message : String(error);
  return <div className="rounded-lg border border-line bg-surface-2 p-3 text-sm text-ink-2">Couldn't load this: {msg}</div>;
}

export function Segmented<T extends string>({ value, options, onChange }: {
  value: T; options: { value: T; label: string }[]; onChange: (v: T) => void;
}) {
  return (
    <div className="inline-flex rounded-lg border border-line bg-surface-2 p-0.5" role="tablist">
      {options.map((o) => (
        <button
          key={o.value}
          role="tab"
          aria-selected={value === o.value}
          onClick={() => onChange(o.value)}
          className={clsx(
            "rounded-md px-2.5 py-1 text-xs font-medium transition-colors",
            value === o.value ? "bg-surface-1 text-ink-1 shadow-card" : "text-ink-2 hover:text-ink-1",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Legend({ items }: { items: { label: string; color: string }[] }) {
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-2">
      {items.map((i) => (
        <span key={i.label} className="inline-flex items-center gap-1.5">
          <span className="h-2.5 w-2.5 rounded-sm" style={{ background: i.color }} aria-hidden />
          {i.label}
        </span>
      ))}
    </div>
  );
}
