import { useQuery } from "@tanstack/react-query";
import { CircleCheck, CircleX } from "lucide-react";
import { api } from "../api/client";
import { Card, Empty, PageHeader, Spinner } from "../components/ui";
import { fmtDateTime, fmtInt } from "../lib/format";

interface Tenant { tenant_id: string; slug: string; name: string; plan_code: string; subscription_status: string; vehicle_quota: number; vehicles: number; open_alerts: number; users: number }
interface Reject { oem: string; reason: string; rejects: number; last_seen: string }
type Deps = Record<string, string | Record<string, string>>;

function withTenant(tenantId?: string) {
  return tenantId ? { headers: { "X-Tenant-Id": tenantId } } : {};
}

export default function Platform() {
  const tenants = useQuery({ queryKey: ["tenants"], queryFn: () => api<{ items: Tenant[] }>("/admin/tenants") });
  const first = tenants.data?.items[0]?.tenant_id;
  const dq = useQuery({ queryKey: ["dq", first], queryFn: () => api<{ items: Reject[] }>("/analytics/data-quality?hours=24", withTenant(first)), enabled: !!first, refetchInterval: 30_000 });
  const deps = useQuery({ queryKey: ["deps", first], queryFn: () => api<Deps>("/admin/dependencies", withTenant(first)), enabled: !!first, refetchInterval: 10_000 });
  return (
    <>
      <PageHeader title="Platform operations" subtitle="Cross-tenant view for operators: tenants, ingestion data quality and dependency health. Tenant data access requires selecting a tenant (audited)." />
      <Card title="Tenants" pad={false}>
        {tenants.isLoading ? <Spinner /> : (
          <table className="w-full">
            <thead><tr className="border-b border-line"><th className="th">Tenant</th><th className="th">Plan</th><th className="th text-right">Vehicles</th><th className="th text-right">Quota</th><th className="th text-right">Open alerts</th><th className="th text-right">Users</th></tr></thead>
            <tbody className="divide-y divide-line">{tenants.data?.items.map((t) => (
              <tr key={t.tenant_id}><td className="td"><div className="font-medium">{t.name}</div><div className="mono text-ink-3">{t.slug}</div></td>
                <td className="td">{t.plan_code} · {t.subscription_status}</td><td className="td text-right tabular-nums">{fmtInt(t.vehicles)}</td>
                <td className="td text-right tabular-nums">{fmtInt(t.vehicle_quota)}</td><td className="td text-right tabular-nums">{fmtInt(t.open_alerts)}</td>
                <td className="td text-right tabular-nums">{t.users}</td></tr>))}
            </tbody>
          </table>
        )}
      </Card>
      <div className="mt-4 grid gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2" title="Ingestion rejects · last 24 h" subtitle="Records dead-lettered by schema/VIN/range validation, replayable after a fix" pad={false}>
          {dq.isLoading ? <Spinner /> : (dq.data?.items.length ?? 0) === 0 ? <Empty>No rejects.</Empty> : (
            <table className="w-full">
              <thead><tr className="border-b border-line"><th className="th">OEM</th><th className="th">Reason</th><th className="th text-right">Count</th><th className="th text-right">Last</th></tr></thead>
              <tbody className="divide-y divide-line">{dq.data!.items.slice(0, 15).map((r, i) => (
                <tr key={i}><td className="td">{r.oem}</td><td className="td mono text-ink-2">{r.reason}</td><td className="td text-right tabular-nums">{fmtInt(r.rejects)}</td><td className="td text-right text-ink-3">{fmtDateTime(r.last_seen)}</td></tr>))}
              </tbody>
            </table>
          )}
        </Card>
        <Card title="Dependencies" subtitle="As seen by the API (circuit breakers included)">
          {!deps.data ? <Spinner /> : (
            <ul className="grid gap-2 text-sm">
              {Object.entries(deps.data).filter(([, v]) => typeof v === "string").map(([k, v]) => (
                <li key={k} className="flex items-center justify-between">
                  <span className="text-ink-1">{k.replace("_", " ")}</span>
                  <span className="inline-flex items-center gap-1.5 text-ink-2">
                    {v === "up" || v === "llm" ? <CircleCheck className="h-4 w-4" style={{ color: "var(--status-good)" }} /> : v === "down" ? <CircleX className="h-4 w-4" style={{ color: "var(--status-critical)" }} /> : null}
                    {String(v)}</span>
                </li>))}
              {Object.entries((deps.data.breakers as Record<string, string>) ?? {}).map(([k, v]) => (
                <li key={k} className="flex items-center justify-between"><span className="text-ink-1">{k} circuit</span><span className="text-ink-2">{v}</span></li>))}
            </ul>
          )}
          <p className="mt-3 text-xs text-ink-3">Kafka lag, throughput and latency live in Grafana. Last refresh {fmtDateTime(Date.now())}.</p>
        </Card>
      </div>
    </>
  );
}
