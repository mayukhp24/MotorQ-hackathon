import { useQuery } from "@tanstack/react-query";
import { AlertOctagon, Car, DollarSign, Radio, Wrench } from "lucide-react";
import { Link } from "react-router-dom";
import { Area, AreaChart, Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, type Page } from "../api/client";
import { ChartTooltip, useAxis } from "../components/charts";
import { useLive } from "../components/Layout";
import { Card, Empty, ErrorNote, Kpi, Legend, PageHeader, RiskBar, SeverityBadge, Spinner } from "../components/ui";
import { useAuth } from "../lib/auth";
import { COMPONENT_LABEL, fmtAgo, fmtCompact, fmtInt, fmtUsd } from "../lib/format";

interface Summary {
  vehicles: number;
  open_alerts: Record<string, number>;
  risk: { high_risk: number; medium_risk: number; expected_breakdowns_7d: number; savings_opportunity: number; scored_on: string | null };
  open_work_orders: number;
  fleets: { fleet_id: string; name: string; city: string; vehicles: number }[];
  live: { online: number; offline: number; by_status: Record<string, number>; critical: number; eps: number; avg_speed_kmh: number };
}

const SEVERITIES = ["CRITICAL", "HIGH", "MEDIUM", "LOW"] as const;

export default function Overview() {
  const { can } = useAuth();
  const live = useLive();
  const summary = useQuery({ queryKey: ["summary"], queryFn: () => api<Summary>("/fleet/summary"), refetchInterval: 15_000 });
  const s = summary.data;
  const k = live.kpi ?? s?.live;

  return (
    <>
      <PageHeader title="Fleet overview" subtitle="Live state of every connected vehicle, open faults and predicted breakdowns." />
      {summary.error && <ErrorNote error={summary.error} />}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        <Kpi label="Online now" icon={Radio} value={k ? fmtInt(k.online) : "–"}
          hint={s ? `of ${fmtInt(s.vehicles)} vehicles` : undefined} tone="accent" />
        <Kpi label="Driving" icon={Car} value={k ? fmtInt(k.by_status?.DRIVING ?? 0) : "–"}
          hint={k ? `avg ${k.avg_speed_kmh} km/h` : undefined} />
        <Kpi label="Active critical faults" icon={AlertOctagon} value={k ? fmtInt(k.critical) : "–"} tone="critical"
          hint="vehicles, last 30 min" />
        <Kpi label="Open critical alerts" icon={AlertOctagon} value={s ? fmtInt(s.open_alerts.CRITICAL ?? 0) : "–"}
          hint={s ? `${fmtInt(Object.values(s.open_alerts).reduce((a, b) => a + b, 0))} open in total` : undefined} tone="serious" />
        <Kpi label="Predicted breakdowns · 7 d" icon={Wrench} value={s ? fmtInt(s.risk.expected_breakdowns_7d) : "–"}
          hint={s ? `${fmtInt(s.risk.high_risk)} vehicles above 50% risk` : undefined} tone="warning" />
        <Kpi label="Savings if acted on now" icon={DollarSign} value={s ? fmtUsd(s.risk.savings_opportunity, true) : "–"}
          hint="planned vs roadside repair" tone="good" />
      </div>

      <div className="mt-4 grid gap-4 xl:grid-cols-3">
        <EventRate className="xl:col-span-2" />
        <LiveAlerts />
      </div>
      <div className="mt-4 grid gap-4 xl:grid-cols-3">
        {can("analytics:read") && <AlertTrend className="xl:col-span-2" />}
        <TopDtc />
      </div>
      <div className="mt-4 grid gap-4 xl:grid-cols-3">
        {can("maintenance:read") && <TopRisk className="xl:col-span-2" />}
        {s && <Fleets fleets={s.fleets} />}
      </div>
    </>
  );
}

function EventRate({ className }: { className?: string }) {
  const { eps, kpi } = useLive();
  const ax = useAxis();
  return (
    <Card className={className} title="Telemetry throughput" subtitle="Events per second processed by the stream processors (live, last 3 min)"
      action={<span className="text-sm tabular-nums text-ink-2">{kpi ? `${fmtInt(kpi.eps)} ev/s` : ""}</span>}>
      <div className="h-56">
        {eps.length < 2 ? <Spinner label="Waiting for the live stream" /> : (
          <ResponsiveContainer>
            <AreaChart data={eps} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
              <defs>
                <linearGradient id="epsFill" x1="0" x2="0" y1="0" y2="1">
                  <stop offset="0%" stopColor={ax.colors.series[0]} stopOpacity={0.25} />
                  <stop offset="100%" stopColor={ax.colors.series[0]} stopOpacity={0.02} />
                </linearGradient>
              </defs>
              <CartesianGrid {...ax.grid} />
              <XAxis dataKey="t" {...ax.x} tickFormatter={(t) => new Date(t).toLocaleTimeString("en-GB", { minute: "2-digit", second: "2-digit" })} minTickGap={40} />
              <YAxis {...ax.y} tickFormatter={fmtCompact} />
              <Tooltip content={<ChartTooltip labelFormatter={(l) => new Date(Number(l)).toLocaleTimeString()} valueFormatter={(v) => `${fmtInt(v)} ev/s`} />}
                cursor={{ stroke: ax.colors.axis, strokeWidth: 1 }} />
              <Area type="monotone" dataKey="eps" name="Events/s" stroke={ax.colors.series[0]} strokeWidth={2} fill="url(#epsFill)" isAnimationActive={false} dot={false} activeDot={{ r: 4 }} />
            </AreaChart>
          </ResponsiveContainer>
        )}
      </div>
    </Card>
  );
}

function LiveAlerts() {
  const { alerts } = useLive();
  return (
    <Card title="Live alerts" subtitle="Pushed over WebSocket as they are detected" pad={false}
      action={<Link to="/alerts" className="text-xs font-medium text-accent-ink hover:underline">All alerts</Link>}>
      <ul className="max-h-[252px] divide-y divide-line overflow-y-auto">
        {alerts.length === 0 && <Empty>No new alerts since you opened this page.</Empty>}
        {alerts.map((a) => (
          <li key={a.alert_id} className="flex items-start gap-3 px-4 py-2.5 sm:px-5">
            <SeverityBadge severity={a.severity} />
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm text-ink-1">{a.title}</div>
              <Link to={`/vehicles/${a.vin}`} className="mono text-ink-3 hover:text-accent-ink">{a.vin}</Link>
            </div>
            <span className="shrink-0 text-xs text-ink-3" title={`detected ${((a.detected_ms - a.ts_ms) / 1000).toFixed(1)} s after the event`}>
              {fmtAgo(a.detected_ms)}
            </span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function AlertTrend({ className }: { className?: string }) {
  const ax = useAxis();
  const q = useQuery({ queryKey: ["alert-trend"], queryFn: () => api<{ points: { day: string; severity: string; count: number }[] }>("/analytics/alert-trend?days=14") });
  const byDay = new Map<string, Record<string, number | string>>();
  for (const p of q.data?.points ?? []) {
    const row = byDay.get(p.day) ?? { day: p.day };
    row[p.severity] = p.count;
    byDay.set(p.day, row);
  }
  const data = [...byDay.values()];
  const color: Record<string, string> = { CRITICAL: ax.colors.status.critical, HIGH: ax.colors.status.serious, MEDIUM: ax.colors.status.warning, LOW: ax.colors.neutral };
  return (
    <Card className={className} title="Alerts per day" subtitle="Last 14 days, by severity"
      action={<Legend items={SEVERITIES.map((s) => ({ label: s[0] + s.slice(1).toLowerCase(), color: color[s] }))} />}>
      <div className="h-60">
        {q.isLoading ? <Spinner /> : (
          <ResponsiveContainer>
            <BarChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }} barCategoryGap="22%">
              <CartesianGrid {...ax.grid} />
              <XAxis dataKey="day" {...ax.x} tickFormatter={(d) => new Date(d).toLocaleDateString("en-GB", { day: "2-digit", month: "short" })} />
              <YAxis {...ax.y} tickFormatter={fmtCompact} />
              <Tooltip content={<ChartTooltip labelFormatter={(l) => new Date(String(l)).toDateString()} />} />
              {[...SEVERITIES].reverse().map((sev, i) => (
                <Bar key={sev} dataKey={sev} name={sev[0] + sev.slice(1).toLowerCase()} stackId="a" fill={color[sev]}
                  stroke={ax.colors.surface1} strokeWidth={2} radius={i === SEVERITIES.length - 1 ? [4, 4, 0, 0] : 0} animationDuration={600} animationEasing="ease-out" />
              ))}
            </BarChart>
          </ResponsiveContainer>
        )}
      </div>
    </Card>
  );
}

function TopDtc() {
  const q = useQuery({ queryKey: ["top-dtc"], queryFn: () => api<{ items: { code: string; count: number }[] }>("/live/top-dtc"), refetchInterval: 10_000 });
  const items = q.data?.items ?? [];
  const max = Math.max(1, ...items.map((i) => i.count));
  return (
    <Card title="Trending fault codes" subtitle="Top-K over the last 15 min (Count-Min Sketch)">
      {items.length === 0 ? <Empty>No fault codes in the current window.</Empty> : (
        <ul className="grid gap-2">
          {items.map((i) => (
            <li key={i.code} className="grid grid-cols-[64px_1fr_40px] items-center gap-2 text-sm">
              <span className="mono text-ink-1">{i.code}</span>
              <div className="h-2 rounded-full bg-surface-2">
                <div className="h-2 rounded-full" style={{ width: `${(i.count / max) * 100}%`, background: "var(--series-1)" }} />
              </div>
              <span className="text-right tabular-nums text-ink-2">{i.count}</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

interface RiskRow { vin: string; plate: string; fleet_name: string; model: string; risk_7d: number; top_component: string; expected_savings_usd: number; top_factors: { label: string }[] }

function TopRisk({ className }: { className?: string }) {
  const q = useQuery({ queryKey: ["risk-top"], queryFn: () => api<Page<RiskRow>>("/maintenance/risk?limit=6&min_risk=0.3") });
  return (
    <Card className={className} title="Most likely to break down this week" pad={false}
      action={<Link to="/maintenance" className="text-xs font-medium text-accent-ink hover:underline">Open predictions</Link>}>
      {q.isLoading ? <Spinner /> : (q.data?.items.length ?? 0) === 0 ? <Empty>No scores yet – the analytics job publishes them daily.</Empty> : (
        <table className="w-full">
          <thead><tr className="border-b border-line"><th className="th">Vehicle</th><th className="th">Risk (7 d)</th><th className="th hidden md:table-cell">Why</th><th className="th text-right">Avoidable cost</th></tr></thead>
          <tbody className="divide-y divide-line">
            {q.data!.items.map((r) => (
              <tr key={r.vin} className="hover:bg-surface-2">
                <td className="td"><Link to={`/vehicles/${r.vin}`} className="mono hover:text-accent-ink">{r.vin}</Link>
                  <div className="text-xs text-ink-3">{COMPONENT_LABEL[r.top_component]} · {r.fleet_name.split("–").pop()}</div></td>
                <td className="td"><RiskBar risk={r.risk_7d} /></td>
                <td className="td hidden text-ink-2 md:table-cell">{r.top_factors[0]?.label ?? "–"}</td>
                <td className="td text-right tabular-nums">{fmtUsd(r.expected_savings_usd)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  );
}

function Fleets({ fleets }: { fleets: Summary["fleets"] }) {
  return (
    <Card title="Fleets" subtitle={`${fleets.length} depots`} pad={false}>
      <ul className="max-h-[300px] divide-y divide-line overflow-y-auto">
        {fleets.map((f) => (
          <li key={f.fleet_id} className="flex items-center justify-between px-4 py-2 text-sm sm:px-5">
            <span className="text-ink-1">{f.city}</span>
            <span className="tabular-nums text-ink-2">{fmtInt(f.vehicles)}</span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

