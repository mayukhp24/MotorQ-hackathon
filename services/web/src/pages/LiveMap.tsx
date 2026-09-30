import { useQuery } from "@tanstack/react-query";
import type { LatLngBounds } from "leaflet";
import "leaflet/dist/leaflet.css";
import { useMemo, useState } from "react";
import { CircleMarker, GeoJSON, MapContainer, TileLayer, Tooltip, useMapEvents } from "react-leaflet";
import { Link } from "react-router-dom";
import { feature } from "topojson-client";
import type { Topology } from "topojson-specification";
import countries from "world-atlas/countries-50m.json";
import { api } from "../api/client";
import { Card, Legend, PageHeader, Segmented, VehicleStatus } from "../components/ui";
import { useAuth } from "../lib/auth";
import { fmtAgo, fmtInt } from "../lib/format";
import { useTheme } from "../lib/theme";

interface Cell { geohash: string; lat: number; lon: number; count: number }
interface LiveVehicle { vin: string; status: string; lat: number; lon: number; speed_kmh: number; critical: boolean; ts_ms: number; soc_pct: number | null; fuel_pct: number | null; coolant_c: number | null }

const STATUS_COLOR: Record<string, string> = { DRIVING: "--status-good", IDLING: "--status-warning", CHARGING: "--accent", PARKED: "--neutral-mark" };

// Offline basemap: country outline from world-atlas (no tile server needed).
const topo = countries as unknown as Topology;
const world = feature(topo, topo.objects.countries) as unknown as GeoJSON.FeatureCollection;
const india = { ...world, features: world.features.filter((f) => String(f.id) === "356") };

function precisionFor(zoom: number) {
  return zoom < 6 ? 3 : zoom < 9 ? 4 : 5;
}

function Viewport({ onChange }: { onChange: (z: number, b: LatLngBounds) => void }) {
  const map = useMapEvents({
    moveend: () => onChange(map.getZoom(), map.getBounds()),
    zoomend: () => onChange(map.getZoom(), map.getBounds()),
  });
  return null;
}

export default function LiveMap() {
  const { resolved, colors } = useTheme();
  const { can } = useAuth();
  const precise = can("location:precise");
  const [zoom, setZoom] = useState(5);
  const [bounds, setBounds] = useState<LatLngBounds | null>(null);
  const [tiles, setTiles] = useState<"on" | "off">("on");
  const detail = zoom >= 11 && bounds;
  const precision = precisionFor(zoom);

  const cells = useQuery({
    queryKey: ["clusters", precision],
    queryFn: () => api<{ cells: Cell[] }>(`/live/clusters?precision=${precision}`),
    refetchInterval: 5000,
    enabled: !detail,
  });
  const bbox = bounds ? [bounds.getWest(), bounds.getSouth(), bounds.getEast(), bounds.getNorth()].map((n) => n.toFixed(4)).join(",") : "";
  const vehicles = useQuery({
    queryKey: ["live-vehicles", bbox],
    queryFn: () => api<{ items: LiveVehicle[]; masked: boolean }>(`/live/vehicles?bbox=${bbox}&limit=1500`),
    refetchInterval: 3000,
    enabled: !!detail,
  });
  const cssVar = (v: string) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
  const statusColor = useMemo(() => Object.fromEntries(Object.entries(STATUS_COLOR).map(([k, v]) => [k, cssVar(v)])),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [resolved]);
  const maxCount = Math.max(1, ...(cells.data?.cells ?? []).map((c) => c.count));
  const total = (cells.data?.cells ?? []).reduce((a, c) => a + c.count, 0);
  const tileUrl = `https://{s}.basemaps.cartocdn.com/${resolved === "dark" ? "dark_all" : "light_all"}/{z}/{x}/{y}{r}.png`;

  return (
    <>
      <PageHeader title="Live map" subtitle={detail
        ? `${fmtInt(vehicles.data?.items.length ?? 0)} vehicles in view · refreshed every 3 s`
        : `${fmtInt(total)} online vehicles, clustered by geohash-${precision} cells · zoom in past level 11 for individual vehicles`}
        actions={<Segmented value={tiles} onChange={setTiles} options={[{ value: "on", label: "Street map" }, { value: "off", label: "Outline only" }]} />} />
      <Card pad={false} className="overflow-hidden">
        <div className="h-[calc(100vh-230px)] min-h-[420px]">
          <MapContainer center={[20.6, 78.9]} zoom={5} minZoom={4} maxZoom={16} className="h-full w-full" preferCanvas
            whenReady={() => undefined}>
            <GeoJSON key={resolved} data={india} style={{ color: colors.axis, weight: 1, fillColor: colors.surface1, fillOpacity: 0.6 }} />
            {tiles === "on" && <TileLayer key={tileUrl} url={tileUrl} attribution='&copy; OpenStreetMap contributors &copy; CARTO' />}
            <Viewport onChange={(z, b) => { setZoom(z); setBounds(b); }} />
            {!detail && (cells.data?.cells ?? []).map((c) => (
              <CircleMarker key={c.geohash} center={[c.lat, c.lon]} radius={6 + Math.sqrt(c.count / maxCount) * 26}
                pathOptions={{ color: colors.surface1, weight: 2, fillColor: colors.series[0], fillOpacity: 0.72 }}>
                <Tooltip direction="top"><strong>{fmtInt(c.count)}</strong> vehicles online · cell {c.geohash}</Tooltip>
              </CircleMarker>
            ))}
            {detail && (vehicles.data?.items ?? []).map((v) => (
              <CircleMarker key={v.vin} center={[v.lat, v.lon]} radius={v.critical ? 8 : 6}
                pathOptions={{ color: v.critical ? colors.status.critical : colors.surface1, weight: 2, fillColor: statusColor[v.status] ?? colors.neutral, fillOpacity: 0.95 }}>
                <Tooltip direction="top" offset={[0, -6]}>
                  <div className="text-xs">
                    <div className="mono font-semibold">{v.vin}</div>
                    <div>{v.status} · {v.speed_kmh ?? 0} km/h · {fmtAgo(v.ts_ms)}</div>
                    {v.coolant_c != null && <div>Coolant {v.coolant_c} °C</div>}
                    {v.soc_pct != null && <div>SoC {v.soc_pct}%</div>}
                    {v.critical && <div style={{ color: colors.status.critical }}>Active critical fault</div>}
                  </div>
                </Tooltip>
              </CircleMarker>
            ))}
          </MapContainer>
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line px-4 py-2.5">
          {detail ? (
            <Legend items={Object.keys(STATUS_COLOR).map((s) => ({ label: s[0] + s.slice(1).toLowerCase(), color: statusColor[s] }))} />
          ) : (
            <Legend items={[{ label: "Online vehicles per cell (area ∝ count)", color: colors.series[0] }]} />
          )}
          <span className="text-xs text-ink-3">
            {precise ? "Precise positions" : "Positions snapped to ~5 km cells for your role (privacy)"}
          </span>
        </div>
      </Card>
      {detail && (vehicles.data?.items.length ?? 0) > 0 && (
        <Card className="mt-4" title="Vehicles in view" pad={false}>
          <div className="max-h-72 overflow-y-auto">
            <table className="w-full">
              <thead><tr className="border-b border-line"><th className="th">VIN</th><th className="th">Status</th><th className="th text-right">Speed</th><th className="th text-right">Last seen</th></tr></thead>
              <tbody className="divide-y divide-line">
                {vehicles.data!.items.slice(0, 200).map((v) => (
                  <tr key={v.vin}><td className="td"><Link className="mono hover:text-accent-ink" to={`/vehicles/${v.vin}`}>{v.vin}</Link></td>
                    <td className="td"><VehicleStatus status={v.status} critical={v.critical} /></td>
                    <td className="td text-right tabular-nums">{v.speed_kmh ?? 0} km/h</td>
                    <td className="td text-right text-ink-2">{fmtAgo(v.ts_ms)}</td></tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </>
  );
}
