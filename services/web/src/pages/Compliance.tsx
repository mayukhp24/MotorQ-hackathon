import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Search, ShieldCheck, ShieldX, UserX } from "lucide-react";
import { useState } from "react";
import { api, qs, type Page } from "../api/client";
import { Card, Empty, ErrorNote, PageHeader, Segmented, Spinner } from "../components/ui";
import { useAuth } from "../lib/auth";
import { fmtDateTime } from "../lib/format";

interface AuditRow { audit_id: number; ts: string; actor_id: string | null; actor_type: string; action: string; resource_type: string; resource_id: string | null; outcome: string; ip: string | null; hash: string }
interface Driver { driver_id: string; full_name: string; status: string; erased_at: string | null; license_last4: string | null; active_assignments: number }
interface Erasure { request_id: string; subject_id: string; status: string; reason: string; requested_at: string; completed_at: string | null; report: Record<string, string> }

export default function Compliance() {
  const { can } = useAuth();
  const [tab, setTab] = useState<"audit" | "erasure">("audit");
  return (
    <>
      <PageHeader title="Compliance" subtitle="Tamper-evident audit trail of every data access and AI action; data-subject erasure (GDPR Art. 17 / DPDP Act s.12)."
        actions={can("privacy:erase") && <Segmented value={tab} onChange={setTab} options={[{ value: "audit", label: "Audit trail" }, { value: "erasure", label: "Right to erasure" }]} />} />
      {tab === "audit" ? <AuditTab /> : <ErasureTab />}
    </>
  );
}

function AuditTab() {
  const [actor, setActor] = useState<"" | "USER" | "AGENT">("");
  const verify = useQuery({ queryKey: ["audit-verify"], queryFn: () => api<{ intact: boolean; first_broken_audit_id: number | null; tenant_rows: number }>("/compliance/audit/verify"), enabled: false });
  const q = useInfiniteQuery({
    queryKey: ["audit", actor],
    queryFn: ({ pageParam }) => api<Page<AuditRow>>(`/compliance/audit${qs({ limit: 50, cursor: pageParam, actor_type: actor })}`),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (l) => l.next_cursor ?? undefined,
  });
  const rows = q.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Card pad={false}>
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-line p-3">
        <Segmented value={actor} onChange={setActor} options={[{ value: "", label: "Everyone" }, { value: "USER", label: "People" }, { value: "AGENT", label: "AI copilot" }]} />
        <div className="flex items-center gap-3">
          {verify.data && (verify.data.intact
            ? <span className="inline-flex items-center gap-1.5 text-sm text-ink-1"><ShieldCheck className="h-4 w-4" style={{ color: "var(--status-good)" }} /> Hash chain intact</span>
            : <span className="inline-flex items-center gap-1.5 text-sm text-ink-1"><ShieldX className="h-4 w-4" style={{ color: "var(--status-critical)" }} /> Chain broken at #{verify.data.first_broken_audit_id}</span>)}
          <button className="btn-outline" onClick={() => verify.refetch()} disabled={verify.isFetching}>{verify.isFetching ? "Verifying…" : "Verify integrity"}</button>
        </div>
      </div>
      {q.isLoading ? <Spinner /> : rows.length === 0 ? <Empty>No audit events.</Empty> : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[900px]">
            <thead><tr className="border-b border-line"><th className="th">#</th><th className="th">Time</th><th className="th">Actor</th><th className="th">Action</th><th className="th">Resource</th><th className="th">Outcome</th><th className="th">Hash</th></tr></thead>
            <tbody className="divide-y divide-line">
              {rows.map((r) => (
                <tr key={r.audit_id}>
                  <td className="td tabular-nums text-ink-3">{r.audit_id}</td>
                  <td className="td whitespace-nowrap">{fmtDateTime(r.ts)}</td>
                  <td className="td">{r.actor_type}<div className="mono text-ink-3">{r.actor_id?.slice(0, 8) ?? "–"}</div></td>
                  <td className="td mono">{r.action}</td>
                  <td className="td text-ink-2">{r.resource_type}{r.resource_id ? <div className="mono text-ink-3">{r.resource_id.slice(0, 17)}</div> : null}</td>
                  <td className="td">{r.outcome}</td>
                  <td className="td mono text-ink-3" title={r.hash}>{r.hash.slice(0, 12)}…</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {q.hasNextPage && <div className="border-t border-line p-3 text-center"><button className="btn-outline" onClick={() => q.fetchNextPage()}>Load more</button></div>}
    </Card>
  );
}

function ErasureTab() {
  const qc = useQueryClient();
  const [term, setTerm] = useState("");
  const [search, setSearch] = useState("");
  const [target, setTarget] = useState<Driver | null>(null);
  const [reason, setReason] = useState("Data subject request received by email");
  const drivers = useQuery({ queryKey: ["drivers", search], queryFn: () => api<{ items: Driver[] }>(`/compliance/drivers${qs({ q: search, limit: 15 })}`) });
  const history = useQuery({ queryKey: ["erasures"], queryFn: () => api<{ items: Erasure[] }>("/compliance/erasure-requests?limit=20") });
  const erase = useMutation({
    mutationFn: () => api<Erasure>("/compliance/erasure-requests", { method: "POST", json: { driver_id: target!.driver_id, reason } }),
    onSuccess: () => { setTarget(null); qc.invalidateQueries({ queryKey: ["drivers"] }); qc.invalidateQueries({ queryKey: ["erasures"] }); },
  });
  return (
    <div className="grid gap-4 xl:grid-cols-2">
      <Card title="Find a data subject" subtitle="Drivers are the personal-data subjects in this system" pad={false}>
        <form className="relative border-b border-line p-3" onSubmit={(e) => { e.preventDefault(); setSearch(term); }}>
          <Search className="pointer-events-none absolute left-5 top-5 h-4 w-4 text-ink-3" />
          <input className="input pl-8" placeholder="Name or driver ID…" value={term} onChange={(e) => setTerm(e.target.value)} aria-label="Search drivers" />
        </form>
        {drivers.isLoading ? <Spinner /> : (
          <ul className="max-h-96 divide-y divide-line overflow-y-auto">
            {(drivers.data?.items ?? []).map((d) => (
              <li key={d.driver_id} className="flex items-center justify-between gap-3 px-4 py-2.5 sm:px-5">
                <div className="min-w-0"><div className="text-sm text-ink-1">{d.full_name}</div>
                  <div className="mono text-ink-3">{d.driver_id.slice(0, 18)}… {d.license_last4 && `· licence …${d.license_last4}`}</div></div>
                {d.status === "ERASED" ? <span className="text-xs text-ink-3">Erased {fmtDateTime(d.erased_at)}</span>
                  : <button className="btn-outline" onClick={() => setTarget(d)}><UserX className="h-3.5 w-3.5" /> Erase</button>}
              </li>
            ))}
          </ul>
        )}
      </Card>
      <div className="grid gap-4">
        {target && (
          <Card title={`Erase ${target.full_name}?`} subtitle="Identity and contact data are destroyed; trips are unlinked and their locations coarsened. This cannot be undone.">
            <label className="label" htmlFor="reason">Reason (kept in the audit trail)</label>
            <input id="reason" className="input mt-1" value={reason} onChange={(e) => setReason(e.target.value)} />
            {erase.error && <div className="mt-2"><ErrorNote error={erase.error} /></div>}
            <div className="mt-3 flex gap-2">
              <button className="btn-primary" style={{ background: "var(--status-critical)" }} onClick={() => erase.mutate()} disabled={erase.isPending || reason.length < 3}>
                <UserX className="h-4 w-4" /> {erase.isPending ? "Erasing…" : "Erase permanently"}</button>
              <button className="btn-ghost" onClick={() => setTarget(null)}>Cancel</button>
            </div>
          </Card>
        )}
        <Card title="Erasure evidence" subtitle="What was done in each data store" pad={false}>
          {(history.data?.items ?? []).length === 0 ? <Empty>No erasure requests yet.</Empty> : (
            <ul className="divide-y divide-line">
              {history.data!.items.map((e) => (
                <li key={e.request_id} className="px-4 py-3 text-sm sm:px-5">
                  <div className="flex justify-between"><span className="font-medium text-ink-1">{e.status}</span><span className="text-xs text-ink-3">{fmtDateTime(e.completed_at ?? e.requested_at)}</span></div>
                  <div className="mono text-ink-3">subject {e.subject_id}</div>
                  <ul className="mt-1.5 grid gap-0.5 text-xs text-ink-2">{Object.entries(e.report).map(([k, v]) => <li key={k}><span className="mono text-ink-1">{k}</span>: {v}</li>)}</ul>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </div>
  );
}
