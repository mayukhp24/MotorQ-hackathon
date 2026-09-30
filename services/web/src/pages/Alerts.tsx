import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Check, CheckCheck } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api, qs, type Page } from "../api/client";
import { Card, Empty, ErrorNote, PageHeader, Segmented, SeverityBadge, Spinner } from "../components/ui";
import { useAuth } from "../lib/auth";
import { fmtAgo, fmtDateTime, titleCase } from "../lib/format";

interface AlertRow {
  alert_id: string; vin: string; alert_type: string; severity: string; title: string; status: string; ts: string;
  detected_at: string; details: Record<string, unknown>; plate: string; fleet_name: string;
}

function detailText(d: Record<string, unknown>) {
  return Object.entries(d).filter(([k]) => !["component"].includes(k)).slice(0, 3).map(([k, v]) => `${k.replace(/_/g, " ")}: ${v}`).join(" · ");
}

export default function Alerts() {
  const { can } = useAuth();
  const qc = useQueryClient();
  const [status, setStatus] = useState<"OPEN" | "ACKNOWLEDGED" | "RESOLVED" | "">("OPEN");
  const [severity, setSeverity] = useState<"" | "CRITICAL" | "HIGH" | "MEDIUM" | "LOW">("");
  const key = ["alerts", status, severity];
  const query = useInfiniteQuery({
    queryKey: key,
    queryFn: ({ pageParam }) => api<Page<AlertRow>>(`/alerts${qs({ limit: 50, cursor: pageParam, status, severity })}`),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (l) => l.next_cursor ?? undefined,
    refetchInterval: 15_000,
  });
  const act = useMutation({
    mutationFn: ({ id, action }: { id: string; action: "ack" | "resolve" }) => api(`/alerts/${id}/${action}`, { method: "POST" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["alerts"] }),
  });
  const rows = query.data?.pages.flatMap((p) => p.items) ?? [];

  return (
    <>
      <PageHeader title="Alerts" subtitle="Detected in real time by the stream processor; persisted idempotently. Acknowledge to take ownership, resolve when fixed." />
      <Card pad={false}>
        <div className="flex flex-wrap items-center gap-2 border-b border-line p-3">
          <Segmented value={status} onChange={setStatus} options={[{ value: "OPEN", label: "Open" }, { value: "ACKNOWLEDGED", label: "Acknowledged" }, { value: "RESOLVED", label: "Resolved" }, { value: "", label: "All" }]} />
          <Segmented value={severity} onChange={setSeverity} options={[{ value: "", label: "Any severity" }, { value: "CRITICAL", label: "Critical" }, { value: "HIGH", label: "High" }, { value: "MEDIUM", label: "Medium" }, { value: "LOW", label: "Low" }]} />
        </div>
        {query.error && <div className="p-3"><ErrorNote error={query.error} /></div>}
        {act.error && <div className="p-3"><ErrorNote error={act.error} /></div>}
        {query.isLoading ? <Spinner /> : rows.length === 0 ? <Empty>No alerts match these filters.</Empty> : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[900px]">
              <thead><tr className="border-b border-line"><th className="th">Severity</th><th className="th">Alert</th><th className="th">Vehicle</th><th className="th">When</th><th className="th">Status</th><th className="th text-right">Action</th></tr></thead>
              <tbody className="divide-y divide-line">
                {rows.map((a) => (
                  <tr key={a.alert_id} className="hover:bg-surface-2">
                    <td className="td"><SeverityBadge severity={a.severity} /></td>
                    <td className="td"><div className="text-ink-1">{a.title}</div><div className="text-xs text-ink-3">{titleCase(a.alert_type)}{Object.keys(a.details || {}).length ? ` · ${detailText(a.details)}` : ""}</div></td>
                    <td className="td"><Link to={`/vehicles/${a.vin}`} className="mono hover:text-accent-ink">{a.vin}</Link><div className="text-xs text-ink-3">{a.plate} · {a.fleet_name.split("–").pop()}</div></td>
                    <td className="td"><div>{fmtDateTime(a.ts)}</div><div className="text-xs text-ink-3">{fmtAgo(a.ts)}</div></td>
                    <td className="td text-ink-2">{titleCase(a.status)}</td>
                    <td className="td text-right">
                      {can("alert:ack") && a.status === "OPEN" && (
                        <button className="btn-outline" onClick={() => act.mutate({ id: a.alert_id, action: "ack" })}><Check className="h-3.5 w-3.5" /> Acknowledge</button>)}
                      {can("alert:ack") && a.status === "ACKNOWLEDGED" && (
                        <button className="btn-outline" onClick={() => act.mutate({ id: a.alert_id, action: "resolve" })}><CheckCheck className="h-3.5 w-3.5" /> Resolve</button>)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {query.hasNextPage && (
          <div className="border-t border-line p-3 text-center">
            <button className="btn-outline" onClick={() => query.fetchNextPage()} disabled={query.isFetchingNextPage}>{query.isFetchingNextPage ? "Loading…" : "Load more"}</button>
          </div>
        )}
      </Card>
    </>
  );
}
