import { useInfiniteQuery } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api, qs, type Page } from "../api/client";
import { Card, Empty, ErrorNote, PageHeader, RiskBar, Segmented, Spinner, VehicleStatus } from "../components/ui";
import { COMPONENT_LABEL, fmtAgo } from "../lib/format";

interface VehicleRow {
  vin: string; plate: string; model_year: number; fleet_name: string; model: string; oem_code: string; powertrain: string;
  risk_7d: number | null; top_component: string | null;
  live: { status: string; speed_kmh: number | null; soc_pct: number | null; fuel_pct: number | null; ts_ms: number; critical: boolean } | null;
}

export default function Vehicles() {
  const [q, setQ] = useState("");
  const [search, setSearch] = useState("");
  const [powertrain, setPowertrain] = useState<"" | "ICE" | "EV" | "HEV">("");
  const [risky, setRisky] = useState<"all" | "risky">("all");
  const query = useInfiniteQuery({
    queryKey: ["vehicles", search, powertrain, risky],
    queryFn: ({ pageParam }) => api<Page<VehicleRow>>(`/vehicles${qs({ limit: 50, cursor: pageParam, q: search, powertrain, risk_min: risky === "risky" ? 0.25 : undefined })}`),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
  const rows = query.data?.pages.flatMap((p) => p.items) ?? [];

  return (
    <>
      <PageHeader title="Vehicles" subtitle="Master data joined with live state and the latest breakdown-risk score. Keyset pagination: constant cost at any depth." />
      <Card pad={false}>
        <div className="flex flex-wrap items-center gap-2 border-b border-line p-3">
          <form className="relative w-full max-w-xs" onSubmit={(e) => { e.preventDefault(); setSearch(q.trim()); }}>
            <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-ink-3" aria-hidden />
            <input className="input pl-8" placeholder="VIN or plate prefix…" value={q} onChange={(e) => setQ(e.target.value)} maxLength={17} aria-label="Search vehicles" />
          </form>
          <Segmented value={powertrain} onChange={setPowertrain}
            options={[{ value: "", label: "All" }, { value: "ICE", label: "ICE" }, { value: "HEV", label: "Hybrid" }, { value: "EV", label: "EV" }]} />
          <Segmented value={risky} onChange={setRisky} options={[{ value: "all", label: "Any risk" }, { value: "risky", label: "Risk ≥ 25%" }]} />
        </div>
        {query.error && <div className="p-3"><ErrorNote error={query.error} /></div>}
        {query.isLoading ? <Spinner /> : rows.length === 0 ? <Empty>No vehicles match.</Empty> : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[860px]">
              <thead><tr className="border-b border-line">
                <th className="th">Vehicle</th><th className="th">Model</th><th className="th">Fleet</th><th className="th">Live</th>
                <th className="th text-right">Speed</th><th className="th text-right">Energy</th><th className="th">Risk (7 d)</th><th className="th">At-risk component</th>
              </tr></thead>
              <tbody className="divide-y divide-line">
                {rows.map((v) => (
                  <tr key={v.vin} className="hover:bg-surface-2">
                    <td className="td"><Link to={`/vehicles/${v.vin}`} className="mono font-medium hover:text-accent-ink">{v.vin}</Link>
                      <div className="text-xs text-ink-3">{v.plate}</div></td>
                    <td className="td"><div>{v.model}</div><div className="text-xs text-ink-3">{v.model_year} · {v.powertrain}</div></td>
                    <td className="td text-ink-2">{v.fleet_name.split("–").pop()}</td>
                    <td className="td"><VehicleStatus status={v.live?.status} critical={v.live?.critical} />
                      <div className="text-xs text-ink-3">{v.live ? fmtAgo(v.live.ts_ms) : "no data"}</div></td>
                    <td className="td text-right tabular-nums">{v.live?.speed_kmh != null ? `${v.live.speed_kmh} km/h` : "–"}</td>
                    <td className="td text-right tabular-nums">{v.live?.soc_pct != null ? `${v.live.soc_pct}% SoC` : v.live?.fuel_pct != null ? `${v.live.fuel_pct}% fuel` : "–"}</td>
                    <td className="td"><RiskBar risk={v.risk_7d} /></td>
                    <td className="td text-ink-2">{v.top_component && (v.risk_7d ?? 0) >= 0.1 ? COMPONENT_LABEL[v.top_component] : "–"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {query.hasNextPage && (
          <div className="border-t border-line p-3 text-center">
            <button className="btn-outline" onClick={() => query.fetchNextPage()} disabled={query.isFetchingNextPage}>
              {query.isFetchingNextPage ? "Loading…" : "Load more"}
            </button>
          </div>
        )}
      </Card>
    </>
  );
}
