import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Wrench } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Bar, BarChart, CartesianGrid, Cell, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, qs, type Page } from "../api/client";
import { ChartTooltip, useAxis } from "../components/charts";
import { Card, Empty, ErrorNote, Kpi, PageHeader, RiskBar, Segmented, Spinner } from "../components/ui";
import { useAuth } from "../lib/auth";
import { COMPONENT_LABEL, fmtCompact, fmtDateTime, fmtInt, fmtUsd, titleCase } from "../lib/format";

interface RiskRow {
  vin: string; plate: string; fleet_name: string; model: string; powertrain: string; risk_7d: number; top_component: string;
  top_factors: { label: string }[]; est_breakdown_cost: number; expected_savings_usd: number; has_open_work_order: boolean;
}
interface Summary {
  by_component: { component: string; high: number; elevated: number; expected_failures_7d: number; savings_usd: number }[];
  distribution: { risk_from: number; risk_to: number; vehicles: number }[];
  model: { model_version: string; trained_at: string; algorithm: string; metrics: ModelMetrics } | null;
  breakdown_history: { week: string; breakdowns: number; cost_usd: number }[];
}
interface MethodMetrics { pr_auc: number; roc_auc: number; precision_at_k: number; recall_at_k: number; net_savings_usd_in_test_window: number }
interface ModelMetrics {
  test_rows: number; positives: number; base_rate: number; k: number; capacity_share: number;
  methods: Record<string, MethodMetrics>; lift_vs_rules: { precision_at_k: number; pr_auc: number };
  train_period: string[]; test_period: string[]; calibration: { decile: number; predicted: number; observed: number }[];
}

export default function Maintenance() {
  const [tab, setTab] = useState<"risk" | "orders" | "model">("risk");
  const summary = useQuery({ queryKey: ["risk-summary"], queryFn: () => api<Summary>("/maintenance/summary") });
  const s = summary.data;
  const totals = s?.by_component.reduce((a, c) => ({ exp: a.exp + Number(c.expected_failures_7d), high: a.high + c.high, sav: a.sav + Number(c.savings_usd || 0) }), { exp: 0, high: 0, sav: 0 });
  return (
    <>
      <PageHeader title="Predictive maintenance" subtitle="Probability each vehicle breaks down within 7 days, which component, why, and what acting now saves."
        actions={<Segmented value={tab} onChange={setTab} options={[{ value: "risk", label: "Predictions" }, { value: "orders", label: "Work orders" }, { value: "model", label: "Model quality" }]} />} />
      {summary.error && <ErrorNote error={summary.error} />}
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Kpi label="Expected breakdowns · 7 d" value={totals ? fmtInt(totals.exp) : "–"} hint="sum of probabilities" tone="warning" />
        <Kpi label="Vehicles above 50%" value={totals ? fmtInt(totals.high) : "–"} hint="inspect within 2 days" tone="critical" />
        <Kpi label="Avoidable cost" value={totals ? fmtUsd(totals.sav, true) : "–"} hint="risk ≥ 25%, planned vs roadside" tone="good" />
        <Kpi label="Model precision @ capacity" value={s?.model ? `${Math.round(s.model.metrics.methods.model.precision_at_k * 100)}%` : "–"}
          hint={s?.model ? `vs ${Math.round(s.model.metrics.methods.threshold_alert_rules.precision_at_k * 100)}% for alert rules` : undefined} tone="accent" />
      </div>
      <div className="mt-4">
        {tab === "risk" && <RiskTab summary={s} />}
        {tab === "orders" && <OrdersTab />}
        {tab === "model" && <ModelTab summary={s} />}
      </div>
    </>
  );
}

function RiskTab({ summary }: { summary?: Summary }) {
  const ax = useAxis();
  const { can } = useAuth();
  const qc = useQueryClient();
  const [component, setComponent] = useState<"" | "cooling" | "battery12v" | "misfire" | "ev_pack">("");
  const q = useInfiniteQuery({
    queryKey: ["risk", component],
    queryFn: ({ pageParam }) => api<Page<RiskRow>>(`/maintenance/risk${qs({ limit: 25, cursor: pageParam, min_risk: 0.1, component })}`),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (l) => l.next_cursor ?? undefined,
  });
  const create = useMutation({
    mutationFn: (r: RiskRow) => api("/maintenance/work-orders", { method: "POST", json: { vin: r.vin, component: r.top_component, priority: r.risk_7d >= 0.5 ? "P1" : "P2", source: "PREDICTION", notes: r.top_factors.map((f) => f.label).join("; ") } }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["risk"] }); qc.invalidateQueries({ queryKey: ["work-orders"] }); },
  });
  const rows = q.data?.pages.flatMap((p) => p.items) ?? [];
  const dist = (summary?.distribution ?? []).map((d) => ({ ...d, band: `${Math.round(d.risk_from * 100)}–${Math.round(d.risk_to * 100)}%` }));
  const compData = (summary?.by_component ?? []).map((c) => ({ ...c, name: COMPONENT_LABEL[c.component] ?? c.component, expected: Number(c.expected_failures_7d) }));
  return (
    <div className="grid gap-4">
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="Expected breakdowns by component" subtitle="Sum of 7-day probabilities">
          <div className="h-56">
            <ResponsiveContainer>
              <BarChart data={compData} layout="vertical" margin={{ top: 4, right: 16, bottom: 0, left: 8 }} barCategoryGap="28%">
                <CartesianGrid {...ax.grid} horizontal={false} vertical />
                <XAxis type="number" {...ax.x} />
                <YAxis type="category" dataKey="name" {...ax.y} width={150} />
                <Tooltip content={<ChartTooltip valueFormatter={(v) => v.toFixed(1)} />} />
                <Bar dataKey="expected" name="Expected breakdowns" fill={ax.colors.series[0]} radius={[0, 4, 4, 0]} animationDuration={600} animationEasing="ease-out" />
              </BarChart>
            </ResponsiveContainer>
          </div>
        </Card>
        <Card title="Risk distribution" subtitle="Vehicles per 10-point risk band (most vehicles are healthy)">
          <div className="h-56">
            <ResponsiveContainer>
              <BarChart data={dist} margin={{ top: 4, right: 8, bottom: 0, left: 0 }} barCategoryGap="12%">
                <CartesianGrid {...ax.grid} />
                <XAxis dataKey="band" {...ax.x} interval={0} tick={{ fill: ax.colors.text3, fontSize: 10 }} />
                <YAxis {...ax.y} width={56} scale="sqrt" tickFormatter={fmtCompact} />
                <Tooltip content={<ChartTooltip labelFormatter={(l) => `Risk ${l}`} valueFormatter={(v) => `${fmtInt(v)} vehicles`} />} />
                <Bar dataKey="vehicles" name="Vehicles" radius={[4, 4, 0, 0]} isAnimationActive={false}>
                  {dist.map((d) => <Cell key={d.band} fill={d.risk_from >= 0.5 ? ax.colors.status.critical : d.risk_from >= 0.2 ? ax.colors.status.serious : ax.colors.series[0]} />)}
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          </div>
          <p className="mt-1 text-xs text-ink-3">Square-root scale so the high-risk tail is visible. Red = above 50%, orange = 20–50%.</p>
        </Card>
      </div>
      <Card title="Ranked vehicles" pad={false}
        action={<Segmented value={component} onChange={setComponent} options={[{ value: "", label: "All" }, { value: "cooling", label: "Cooling" }, { value: "battery12v", label: "12 V" }, { value: "misfire", label: "Misfire" }, { value: "ev_pack", label: "HV pack" }]} />}>
        {create.error && <div className="p-3"><ErrorNote error={create.error} /></div>}
        {q.isLoading ? <Spinner /> : rows.length === 0 ? <Empty>No vehicles above 10% risk.</Empty> : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[980px]">
              <thead><tr className="border-b border-line"><th className="th">Vehicle</th><th className="th">Risk (7 d)</th><th className="th">Component</th><th className="th">Why</th><th className="th text-right">Avoidable</th><th className="th text-right">Action</th></tr></thead>
              <tbody className="divide-y divide-line">
                {rows.map((r) => (
                  <tr key={r.vin} className="hover:bg-surface-2">
                    <td className="td"><Link to={`/vehicles/${r.vin}`} className="mono hover:text-accent-ink">{r.vin}</Link><div className="text-xs text-ink-3">{r.model} · {r.fleet_name.split("–").pop()}</div></td>
                    <td className="td"><RiskBar risk={r.risk_7d} /></td>
                    <td className="td text-ink-2">{COMPONENT_LABEL[r.top_component]}</td>
                    <td className="td"><ul className="text-xs text-ink-2">{r.top_factors.slice(0, 2).map((f) => <li key={f.label}>• {f.label}</li>)}</ul></td>
                    <td className="td text-right tabular-nums">{fmtUsd(r.expected_savings_usd)}</td>
                    <td className="td text-right">
                      {r.has_open_work_order ? <span className="text-xs text-ink-3">Work order open</span> : can("maintenance:write") && (
                        <button className="btn-outline" disabled={create.isPending} onClick={() => create.mutate(r)}><Wrench className="h-3.5 w-3.5" /> Schedule</button>)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {q.hasNextPage && <div className="border-t border-line p-3 text-center"><button className="btn-outline" onClick={() => q.fetchNextPage()}>Load more</button></div>}
      </Card>
    </div>
  );
}

interface WorkOrder { work_order_id: string; vin: string; source: string; component: string | null; priority: string; status: string; due_on: string | null; est_cost_usd: number | null; notes: string | null; created_at: string; version: number }

function OrdersTab() {
  const qc = useQueryClient();
  const { can } = useAuth();
  const q = useQuery({ queryKey: ["work-orders"], queryFn: () => api<Page<WorkOrder>>("/maintenance/work-orders?limit=100") });
  const update = useMutation({
    mutationFn: ({ wo, status }: { wo: WorkOrder; status: string }) => api(`/maintenance/work-orders/${wo.work_order_id}`, { method: "PATCH", json: { status }, headers: { "If-Match": String(wo.version) } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["work-orders"] }),
  });
  const next: Record<string, string> = { OPEN: "SCHEDULED", SCHEDULED: "IN_PROGRESS", IN_PROGRESS: "DONE" };
  return (
    <Card title="Work orders" subtitle="Updates use optimistic concurrency (If-Match version) so two managers can't overwrite each other" pad={false}>
      {update.error && <div className="p-3"><ErrorNote error={update.error} /></div>}
      {q.isLoading ? <Spinner /> : (q.data?.items.length ?? 0) === 0 ? <Empty>No work orders yet. Schedule one from the predictions tab or approve a copilot proposal.</Empty> : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[860px]">
            <thead><tr className="border-b border-line"><th className="th">Vehicle</th><th className="th">Priority</th><th className="th">Component</th><th className="th">Source</th><th className="th">Status</th><th className="th">Created</th><th className="th text-right">Est. cost</th><th className="th text-right">Action</th></tr></thead>
            <tbody className="divide-y divide-line">
              {q.data!.items.map((w) => (
                <tr key={w.work_order_id}>
                  <td className="td"><Link to={`/vehicles/${w.vin}`} className="mono hover:text-accent-ink">{w.vin}</Link></td>
                  <td className="td">{w.priority}</td>
                  <td className="td text-ink-2">{w.component ? COMPONENT_LABEL[w.component] : "–"}</td>
                  <td className="td text-ink-2">{titleCase(w.source)}</td>
                  <td className="td">{titleCase(w.status)}</td>
                  <td className="td text-ink-2">{fmtDateTime(w.created_at)}</td>
                  <td className="td text-right tabular-nums">{fmtUsd(w.est_cost_usd)}</td>
                  <td className="td text-right">{can("maintenance:write") && next[w.status] && (
                    <button className="btn-outline" onClick={() => update.mutate({ wo: w, status: next[w.status] })}>Mark {titleCase(next[w.status]).toLowerCase()}</button>)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

const METHOD_LABEL: Record<string, string> = { model: "FleetPulse model", threshold_alert_rules: "Threshold alert rules", any_dtc_last_7d: "Any fault code in 7 days" };

function ModelTab({ summary }: { summary?: Summary }) {
  const ax = useAxis();
  const m = summary?.model;
  if (!m) return <Card><Empty>No active model yet.</Empty></Card>;
  const mm = m.metrics;
  const bars = Object.entries(mm.methods).map(([k, v]) => ({ name: METHOD_LABEL[k] ?? k, "PR-AUC": v.pr_auc, "Precision @ capacity": v.precision_at_k, "Recall @ capacity": v.recall_at_k }));
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Card title="Model vs today's rules" subtitle={`Held-out period ${mm.test_period.join(" → ")}: ${fmtInt(mm.test_rows)} vehicle-days, ${fmtInt(mm.positives)} breakdowns (base rate ${(mm.base_rate * 100).toFixed(1)}%)`}>
        <div className="h-64">
          <ResponsiveContainer>
            <BarChart data={bars} margin={{ top: 8, right: 8, bottom: 0, left: 0 }} barCategoryGap="24%">
              <CartesianGrid {...ax.grid} />
              <XAxis dataKey="name" {...ax.x} tick={{ fill: ax.colors.text3, fontSize: 11 }} />
              <YAxis {...ax.y} domain={[0, 1]} tickFormatter={(v) => `${Math.round(v * 100)}%`} />
              <Tooltip content={<ChartTooltip valueFormatter={(v) => `${(v * 100).toFixed(1)}%`} />} />
              {["PR-AUC", "Precision @ capacity", "Recall @ capacity"].map((k, i) => (
                <Bar key={k} dataKey={k} name={k} fill={ax.colors.series[i]} radius={[4, 4, 0, 0]} isAnimationActive={false} />
              ))}
            </BarChart>
          </ResponsiveContainer>
        </div>
        <div className="mt-2 flex flex-wrap gap-4 text-xs text-ink-2">
          {["PR-AUC", "Precision @ capacity", "Recall @ capacity"].map((k, i) => (
            <span key={k} className="inline-flex items-center gap-1.5"><span className="h-2.5 w-2.5 rounded-sm" style={{ background: ax.colors.series[i] }} />{k}</span>))}
        </div>
      </Card>
      <Card title="Scorecard" subtitle={`${m.model_version} · ${m.algorithm}`} pad={false}>
        <table className="w-full">
          <thead><tr className="border-b border-line"><th className="th">Method</th><th className="th text-right">PR-AUC</th><th className="th text-right">ROC-AUC</th><th className="th text-right">Precision@k</th><th className="th text-right">Net saving (test)</th></tr></thead>
          <tbody className="divide-y divide-line">
            {Object.entries(mm.methods).map(([k, v]) => (
              <tr key={k}><td className="td font-medium">{METHOD_LABEL[k] ?? k}</td><td className="td text-right tabular-nums">{v.pr_auc.toFixed(3)}</td>
                <td className="td text-right tabular-nums">{v.roc_auc.toFixed(3)}</td><td className="td text-right tabular-nums">{(v.precision_at_k * 100).toFixed(1)}%</td>
                <td className="td text-right tabular-nums">{fmtUsd(v.net_savings_usd_in_test_window, true)}</td></tr>
            ))}
          </tbody>
        </table>
        <div className="p-4 text-sm text-ink-2 sm:p-5">
          Workshop capacity fixed at {(mm.capacity_share * 100).toFixed(0)}% of vehicle-days (k = {fmtInt(mm.k)}) for every method.
          Lift vs rules: <strong className="text-ink-1">{mm.lift_vs_rules.precision_at_k}×</strong> precision, <strong className="text-ink-1">{mm.lift_vs_rules.pr_auc}×</strong> PR-AUC.
          Trained on {mm.train_period.join(" → ")}; evaluated only on later days (no leakage).
        </div>
      </Card>
      <Card className="lg:col-span-2" title="Calibration" subtitle="Predicted vs observed breakdown rate by risk decile – a calibrated model's bars match">
        <div className="h-56">
          <ResponsiveContainer>
            <BarChart data={mm.calibration.map((c) => ({ ...c, decile: `D${c.decile + 1}` }))} margin={{ top: 8, right: 8, bottom: 0, left: 0 }} barCategoryGap="20%">
              <CartesianGrid {...ax.grid} />
              <XAxis dataKey="decile" {...ax.x} />
              <YAxis {...ax.y} tickFormatter={(v) => `${Math.round(v * 100)}%`} />
              <Tooltip content={<ChartTooltip valueFormatter={(v) => `${(v * 100).toFixed(2)}%`} />} />
              <Bar dataKey="predicted" name="Predicted" fill={ax.colors.series[0]} radius={[4, 4, 0, 0]} isAnimationActive={false} />
              <Bar dataKey="observed" name="Observed" fill={ax.colors.series[1]} radius={[4, 4, 0, 0]} isAnimationActive={false} />
            </BarChart>
          </ResponsiveContainer>
        </div>
        <div className="mt-2 flex gap-4 text-xs text-ink-2">
          <span className="inline-flex items-center gap-1.5"><span className="h-2.5 w-2.5 rounded-sm" style={{ background: ax.colors.series[0] }} />Predicted</span>
          <span className="inline-flex items-center gap-1.5"><span className="h-2.5 w-2.5 rounded-sm" style={{ background: ax.colors.series[1] }} />Observed</span>
        </div>
      </Card>
    </div>
  );
}
