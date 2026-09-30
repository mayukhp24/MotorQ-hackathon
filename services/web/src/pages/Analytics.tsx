import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Cell, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api } from "../api/client";
import { ChartTooltip, useAxis } from "../components/charts";
import { Card, Empty, Kpi, PageHeader, Segmented, Spinner } from "../components/ui";
import { fmtCompact, fmtInt, fmtUsd } from "../lib/format";

interface Idle {
  days: number;
  total: { idle_h: number; cost_usd: number; vehicles: number; savings_at_30pct_usd: number; annualised_cost_usd: number };
  top_vehicles: { vin: string; idle_h: number; cost_usd: number; combustion: number }[];
  daily: { day: string; idle_h: number; approx_cost_usd: number }[];
}
interface Drivers {
  days: number; drivers: number; avg_score: number; refreshed_at: string;
  riskiest: { driver_id: string; name: string; score: number; km: number; trips: number; harsh_per_100km: number; overspeed_s: number }[];
  distribution: { score_from: number; drivers: number }[];
}
interface Hourly { points: { t: number; events: number; vehicles: number; avg_speed_kmh: number; harsh_events: number; avg_pipeline_latency_ms: number }[]; degraded: boolean }

export default function Analytics() {
  const [days, setDays] = useState<"7" | "30">("7");
  return (
    <>
      <PageHeader title="Cost & safety" subtitle="Batch analytics over the historical store: where money leaks and which drivers need coaching."
        actions={<Segmented value={days} onChange={setDays} options={[{ value: "7", label: "7 days" }, { value: "30", label: "30 days" }]} />} />
      <IdleSection days={Number(days)} />
      <div className="mt-4 grid gap-4 xl:grid-cols-2">
        <DriverSection days={Number(days)} />
        <ActivitySection />
      </div>
    </>
  );
}

function IdleSection({ days }: { days: number }) {
  const ax = useAxis();
  const q = useQuery({ queryKey: ["idle", days], queryFn: () => api<Idle>(`/analytics/idle-cost?days=${days}`) });
  const d = q.data;
  return (
    <>
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Kpi label={`Idle hours · ${days} d`} value={d ? fmtCompact(d.total.idle_h) : "–"} hint={d ? `${fmtInt(d.total.vehicles)} vehicles` : undefined} />
        <Kpi label={`Idle cost · ${days} d`} value={d ? fmtUsd(d.total.cost_usd, true) : "–"} tone="serious" hint="fuel ≈ 0.9 L/h · EV HVAC ≈ 2 kW" />
        <Kpi label="Annualised" value={d ? fmtUsd(d.total.annualised_cost_usd, true) : "–"} tone="warning" />
        <Kpi label="Saving at −30% idling" value={d ? fmtUsd(d.total.savings_at_30pct_usd, true) : "–"} tone="good" hint={`per ${days} days`} />
      </div>
      <div className="mt-4 grid gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2" title="Idle hours per day" subtitle="Engine on, stationary, not charging">
          <div className="h-56">
            {q.isLoading ? <Spinner /> : (
              <ResponsiveContainer>
                <BarChart data={d?.daily ?? []} margin={{ top: 8, right: 8, bottom: 0, left: 0 }} barCategoryGap="20%">
                  <CartesianGrid {...ax.grid} />
                  <XAxis dataKey="day" {...ax.x} tickFormatter={(v) => new Date(v).toLocaleDateString("en-GB", { day: "2-digit", month: "short" })} />
                  <YAxis {...ax.y} tickFormatter={fmtCompact} />
                  <Tooltip content={<ChartTooltip labelFormatter={(l) => new Date(String(l)).toDateString()} valueFormatter={(v) => `${fmtInt(v)} h`} />} />
                  <Bar dataKey="idle_h" name="Idle hours" fill={ax.colors.series[1]} radius={[4, 4, 0, 0]} isAnimationActive={false} />
                </BarChart>
              </ResponsiveContainer>
            )}
          </div>
        </Card>
        <Card title="Top idling vehicles" pad={false}>
          {(d?.top_vehicles ?? []).length === 0 ? <Empty>No data.</Empty> : (
            <table className="w-full">
              <thead><tr className="border-b border-line"><th className="th">VIN</th><th className="th text-right">Hours</th><th className="th text-right">Cost</th></tr></thead>
              <tbody className="divide-y divide-line">{d!.top_vehicles.slice(0, 8).map((v) => (
                <tr key={v.vin}><td className="td"><Link to={`/vehicles/${v.vin}`} className="mono hover:text-accent-ink">{v.vin}</Link></td>
                  <td className="td text-right tabular-nums">{v.idle_h}</td><td className="td text-right tabular-nums">{fmtUsd(v.cost_usd)}</td></tr>))}
              </tbody>
            </table>
          )}
        </Card>
      </div>
    </>
  );
}

function DriverSection({ days }: { days: number }) {
  const ax = useAxis();
  const q = useQuery({ queryKey: ["drivers", days], queryFn: () => api<Drivers>(`/analytics/driver-safety?days=${days}&limit=8`) });
  const d = q.data;
  return (
    <Card title="Driver safety" subtitle={d ? `${fmtInt(d.drivers)} drivers with ≥ 50 km · fleet average ${d.avg_score}/100 · harsh events and overspeed per 100 km` : undefined}>
      {q.isLoading ? <Spinner /> : !d ? <Empty>No data.</Empty> : (
        <>
          <div className="h-44">
            <ResponsiveContainer>
              <BarChart data={d.distribution.map((x) => ({ ...x, band: `${x.score_from}–${x.score_from + 10}` }))} margin={{ top: 8, right: 8, bottom: 0, left: 0 }} barCategoryGap="10%">
                <CartesianGrid {...ax.grid} />
                <XAxis dataKey="band" {...ax.x} interval={0} tick={{ fill: ax.colors.text3, fontSize: 10 }} />
                <YAxis {...ax.y} scale="sqrt" tickFormatter={fmtCompact} />
                <Tooltip content={<ChartTooltip labelFormatter={(l) => `Score ${l}`} valueFormatter={(v) => `${fmtInt(v)} drivers`} />} />
                <Bar dataKey="drivers" name="Drivers" radius={[4, 4, 0, 0]} isAnimationActive={false}>
                  {d.distribution.map((x) => <Cell key={x.score_from} fill={x.score_from < 60 ? ax.colors.status.serious : ax.colors.series[0]} />)}
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          </div>
          <div className="label mb-1 mt-3">Coaching priority</div>
          <table className="w-full">
            <tbody className="divide-y divide-line">{d.riskiest.map((r) => (
              <tr key={r.driver_id}><td className="td">{r.name}</td><td className="td text-right tabular-nums">{r.score}/100</td>
                <td className="td text-right text-ink-2 tabular-nums">{r.harsh_per_100km} harsh/100 km</td><td className="td text-right text-ink-3 tabular-nums">{fmtInt(r.km)} km</td></tr>))}
            </tbody>
          </table>
        </>
      )}
    </Card>
  );
}

function ActivitySection() {
  const ax = useAxis();
  const q = useQuery({ queryKey: ["hourly"], queryFn: () => api<Hourly>("/analytics/fleet-hourly?hours=24"), refetchInterval: 60_000 });
  return (
    <Card title="Fleet activity · last 24 h" subtitle={q.data?.degraded ? "Analytics store unavailable" : "Events ingested per hour (ClickHouse rollup)"}>
      {q.isLoading ? <Spinner /> : (q.data?.points.length ?? 0) === 0 ? <Empty>No activity recorded yet.</Empty> : (
        <div className="h-72">
          <ResponsiveContainer>
            <AreaChart data={q.data!.points} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
              <CartesianGrid {...ax.grid} />
              <XAxis dataKey="t" {...ax.x} tickFormatter={(t) => new Date(t).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })} minTickGap={30} />
              <YAxis {...ax.y} tickFormatter={fmtCompact} />
              <Tooltip content={<ChartTooltip labelFormatter={(l) => new Date(Number(l)).toLocaleString()} valueFormatter={(v) => fmtInt(v)} />} cursor={{ stroke: ax.colors.axis }} />
              <Area type="monotone" dataKey="events" name="Events" stroke={ax.colors.series[0]} strokeWidth={2} fill={ax.colors.series[0]} fillOpacity={0.12} isAnimationActive={false} dot={false} />
            </AreaChart>
          </ResponsiveContainer>
        </div>
      )}
    </Card>
  );
}
