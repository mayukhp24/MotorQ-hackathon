import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Wrench } from "lucide-react";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { CartesianGrid, Line, LineChart, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api } from "../api/client";
import { ChartTooltip, useAxis } from "../components/charts";
import { Card, Empty, ErrorNote, PageHeader, RiskBar, Segmented, SeverityBadge, Spinner, VehicleStatus } from "../components/ui";
import { useAuth } from "../lib/auth";
import { COMPONENT_LABEL, fmtAgo, fmtDateTime, fmtUsd, titleCase } from "../lib/format";

interface Detail {
  vin: string; plate: string; model_year: number; fleet_name: string; depot_name: string; city: string; model: string; oem_name: string;
  powertrain: string; body_type: string; battery_kwh: number | null; tank_l: number | null;
  driver: { driver_id: string; name: string; status: string } | null;
  risk: { risk_7d: number; top_component: string; top_factors: { label: string }[]; est_breakdown_cost: number; expected_savings_usd: number; scored_on: string; model_version: string } | null;
  recent_alerts: { alert_id: string; alert_type: string; severity: string; title: string; status: string; ts: string }[];
  service_history: { event_type: string; component: string; occurred_at: string; cost_usd: number; notes: string }[];
  work_orders: { work_order_id: string; source: string; component: string; priority: string; status: string; created_at: string; est_cost_usd: number | null }[];
  live: { status: string; speed_kmh: number | null; coolant_c: number | null; batt_v: number | null; soc_pct: number | null; fuel_pct: number | null; pack_temp_c: number | null; odo_km: number | null; ts_ms: number; critical: boolean; lat: number | null; lon: number | null } | null;
}
type Point = Record<string, number | null>;

export default function VehicleDetail() {
  const { vin = "" } = useParams();
  const { can } = useAuth();
  const qc = useQueryClient();
  const [minutes, setMinutes] = useState<"60" | "240" | "1440">("240");
  const detail = useQuery({ queryKey: ["vehicle", vin], queryFn: () => api<Detail>(`/vehicles/${vin}`), refetchInterval: 10_000 });
  const tel = useQuery({ queryKey: ["telemetry", vin, minutes], queryFn: () => api<{ points: Point[]; degraded: boolean }>(`/vehicles/${vin}/telemetry?minutes=${minutes}`), refetchInterval: 15_000 });
  const daily = useQuery({ queryKey: ["daily", vin], queryFn: () => api<{ days: Point[] }>(`/vehicles/${vin}/daily?days=30`) });
  const createWo = useMutation({
    mutationFn: () => api("/maintenance/work-orders", { method: "POST", json: { vin, component: d?.risk?.top_component, priority: (d?.risk?.risk_7d ?? 0) >= 0.5 ? "P1" : "P2", source: "PREDICTION", notes: d?.risk?.top_factors.map((f) => f.label).join("; ") } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["vehicle", vin] }),
  });
  const d = detail.data;
  if (detail.isLoading) return <Spinner />;
  if (detail.error || !d) return <ErrorNote error={detail.error ?? "not found"} />;
  const isEV = d.powertrain === "EV";
  const openWo = d.work_orders.some((w) => ["OPEN", "SCHEDULED", "IN_PROGRESS"].includes(w.status));

  return (
    <>
      <Link to="/vehicles" className="mb-3 inline-flex items-center gap-1 text-sm text-ink-2 hover:text-ink-1"><ArrowLeft className="h-4 w-4" /> Vehicles</Link>
      <PageHeader title={`${d.model} · ${d.plate}`}
        subtitle={<><span className="mono">{d.vin}</span> · {d.oem_name} · {d.model_year} · {d.powertrain} · {d.fleet_name}{d.driver && <> · Driver: {d.driver.name}</>}</>} />

      <div className="grid gap-4 lg:grid-cols-3">
        <Card title="Live" subtitle={d.live ? `Last report ${fmtAgo(d.live.ts_ms)}` : "No live data"}>
          {d.live ? (
            <dl className="grid grid-cols-2 gap-x-4 gap-y-3 text-sm">
              <div className="col-span-2"><VehicleStatus status={d.live.status} critical={d.live.critical} /></div>
              <Stat label="Speed" value={d.live.speed_kmh != null ? `${d.live.speed_kmh} km/h` : "–"} />
              <Stat label="Odometer" value={d.live.odo_km != null ? `${Math.round(d.live.odo_km).toLocaleString()} km` : "–"} />
              {!isEV && <Stat label="Coolant" value={d.live.coolant_c != null ? `${d.live.coolant_c} °C` : "–"} />}
              {d.live.pack_temp_c != null && <Stat label="HV pack" value={`${d.live.pack_temp_c} °C`} />}
              <Stat label="12 V system" value={d.live.batt_v != null ? `${d.live.batt_v} V` : "–"} />
              <Stat label={isEV ? "State of charge" : "Fuel"} value={isEV ? `${d.live.soc_pct ?? "–"}%` : `${d.live.fuel_pct ?? "–"}%`} />
            </dl>
          ) : <Empty>Vehicle has not reported in the live window.</Empty>}
        </Card>

        <Card className="lg:col-span-2" title="Breakdown risk – next 7 days"
          subtitle={d.risk ? `Model ${d.risk.model_version}, scored ${d.risk.scored_on}` : "Not scored yet"}
          action={can("maintenance:write") && d.risk && d.risk.risk_7d >= 0.1 && (
            <button className="btn-primary" disabled={openWo || createWo.isPending} onClick={() => createWo.mutate()}>
              <Wrench className="h-4 w-4" /> {openWo ? "Work order open" : createWo.isPending ? "Creating…" : "Create work order"}
            </button>
          )}>
          {d.risk ? (
            <div className="grid gap-4 sm:grid-cols-[220px_1fr]">
              <div>
                <div className="text-4xl font-semibold tabular-nums text-ink-1">{(d.risk.risk_7d * 100).toFixed(0)}%</div>
                <div className="mt-1"><RiskBar risk={d.risk.risk_7d} /></div>
                <div className="mt-3 text-sm text-ink-2">Most likely failure: <span className="font-medium text-ink-1">{COMPONENT_LABEL[d.risk.top_component]}</span></div>
                <div className="mt-1 text-sm text-ink-2">Roadside breakdown ≈ {fmtUsd(d.risk.est_breakdown_cost)}; expected saving from acting now {fmtUsd(d.risk.expected_savings_usd)}</div>
              </div>
              <div>
                <div className="label mb-2">Why the model thinks so</div>
                {d.risk.top_factors.length ? (
                  <ul className="grid gap-2">{d.risk.top_factors.map((f) => (
                    <li key={f.label} className="rounded-lg border border-line bg-surface-2 px-3 py-2 text-sm text-ink-1">{f.label}</li>
                  ))}</ul>
                ) : <p className="text-sm text-ink-3">No signal deviates from the healthy-fleet baseline.</p>}
                {createWo.isSuccess && <p className="mt-2 text-sm text-ink-2">Work order created.</p>}
              </div>
            </div>
          ) : <Empty>Scores are published daily by the analytics job.</Empty>}
        </Card>
      </div>

      <Card className="mt-4" title="Telemetry" subtitle={tel.data?.degraded ? "History store unavailable – showing live state only" : "Down-sampled from ClickHouse"}
        action={<Segmented value={minutes} onChange={setMinutes} options={[{ value: "60", label: "1 h" }, { value: "240", label: "4 h" }, { value: "1440", label: "24 h" }]} />}>
        {tel.isLoading ? <Spinner /> : (tel.data?.points.length ?? 0) === 0 ? <Empty>No telemetry in this window.</Empty> : (
          <div className="grid gap-4 md:grid-cols-2">
            <Mini title="Speed (km/h)" data={tel.data!.points} k="speed_kmh" color={0} />
            {isEV ? <Mini title="HV pack temperature (°C)" data={tel.data!.points} k="pack_temp_c" color={1} ref1={55} />
              : <Mini title="Coolant temperature (°C)" data={tel.data!.points} k="coolant_c" color={1} ref1={110} />}
            <Mini title="12 V system voltage (V)" data={tel.data!.points} k="batt_v" color={2} ref1={11.8} decimals={2} />
            <Mini title={isEV ? "State of charge (%)" : "Fuel level (%)"} data={tel.data!.points} k={isEV ? "soc_pct" : "fuel_pct"} color={0} />
          </div>
        )}
      </Card>

      <Card className="mt-4" title="30-day health trend" subtitle="Daily aggregates from the feature store (the inputs the model sees)">
        {daily.isLoading ? <Spinner /> : (daily.data?.days.length ?? 0) === 0 ? <Empty>No history.</Empty> : (
          <div className="grid gap-4 md:grid-cols-3">
            {isEV ? <Mini title="Peak HV pack °C / day" data={daily.data!.days} k="max_pack_temp_c" x="day" color={1} />
              : <Mini title="Average coolant °C / day" data={daily.data!.days} k="avg_coolant_c" x="day" color={1} />}
            <Mini title="Minimum system voltage / day" data={daily.data!.days} k="min_batt_v" x="day" color={2} decimals={2} />
            <Mini title="Fault codes / day" data={daily.data!.days} k="dtc_total" x="day" color={3} />
          </div>
        )}
      </Card>

      <div className="mt-4 grid gap-4 lg:grid-cols-3">
        <Card title="Recent alerts" pad={false}>
          {d.recent_alerts.length === 0 ? <Empty>None.</Empty> : (
            <ul className="divide-y divide-line">{d.recent_alerts.map((a) => (
              <li key={a.alert_id} className="px-4 py-2.5 sm:px-5">
                <div className="flex items-center justify-between gap-2"><SeverityBadge severity={a.severity} /><span className="text-xs text-ink-3">{fmtDateTime(a.ts)}</span></div>
                <div className="mt-1 text-sm text-ink-1">{a.title}</div>
                <div className="text-xs text-ink-3">{titleCase(a.alert_type)} · {titleCase(a.status)}</div>
              </li>))}</ul>
          )}
        </Card>
        <Card title="Service history" pad={false}>
          {d.service_history.length === 0 ? <Empty>No breakdowns or repairs recorded.</Empty> : (
            <ul className="divide-y divide-line">{d.service_history.map((s, i) => (
              <li key={i} className="px-4 py-2.5 text-sm sm:px-5">
                <div className="flex justify-between"><span className="font-medium text-ink-1">{titleCase(s.event_type)}</span><span className="text-xs text-ink-3">{fmtDateTime(s.occurred_at)}</span></div>
                <div className="text-ink-2">{COMPONENT_LABEL[s.component] ?? s.component}{s.cost_usd ? ` · ${fmtUsd(s.cost_usd)}` : ""}</div>
              </li>))}</ul>
          )}
        </Card>
        <Card title="Work orders" pad={false}>
          {d.work_orders.length === 0 ? <Empty>No work orders.</Empty> : (
            <ul className="divide-y divide-line">{d.work_orders.map((w) => (
              <li key={w.work_order_id} className="px-4 py-2.5 text-sm sm:px-5">
                <div className="flex justify-between"><span className="font-medium text-ink-1">{w.priority} · {titleCase(w.status)}</span><span className="text-xs text-ink-3">{fmtDateTime(w.created_at)}</span></div>
                <div className="text-ink-2">{COMPONENT_LABEL[w.component] ?? "General"} · source {titleCase(w.source)}</div>
              </li>))}</ul>
          )}
        </Card>
      </div>
    </>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return <div><dt className="label">{label}</dt><dd className="mt-0.5 tabular-nums text-ink-1">{value}</dd></div>;
}

function Mini({ title, data, k, x = "t", color, ref1, decimals = 1 }: {
  title: string; data: Point[]; k: string; x?: string; color: number; ref1?: number; decimals?: number;
}) {
  const ax = useAxis();
  const stroke = ax.colors.series[color];
  const fmtX = (v: unknown) => (x === "t" ? new Date(Number(v)).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })
    : new Date(String(v)).toLocaleDateString("en-GB", { day: "2-digit", month: "short" }));
  return (
    <div>
      <div className="mb-1 text-xs font-medium text-ink-2">{title}</div>
      <div className="h-40">
        <ResponsiveContainer>
          <LineChart data={data} margin={{ top: 6, right: 8, bottom: 0, left: 0 }}>
            <CartesianGrid {...ax.grid} />
            <XAxis dataKey={x} {...ax.x} tickFormatter={fmtX} minTickGap={36} />
            <YAxis {...ax.y} domain={["auto", "auto"]} tickFormatter={(v) => Number(v).toFixed(decimals > 1 ? 1 : 0)} />
            {ref1 != null && <ReferenceLine y={ref1} stroke={ax.colors.status.critical} strokeDasharray="4 3" ifOverflow="extendDomain"
              label={{ value: "alert", position: "insideTopRight", fill: ax.colors.text3, fontSize: 10 }} />}
            <Tooltip content={<ChartTooltip labelFormatter={fmtX} valueFormatter={(v) => Number(v).toFixed(decimals)} />} cursor={{ stroke: ax.colors.axis }} />
            <Line type="monotone" dataKey={k} name={title.split(" (")[0]} stroke={stroke} strokeWidth={2} dot={false} activeDot={{ r: 4 }} connectNulls isAnimationActive={false} />
          </LineChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}
