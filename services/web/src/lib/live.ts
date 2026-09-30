import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";

export interface LiveKpi {
  online: number;
  by_status: Record<string, number>;
  critical: number;
  eps: number;
  moving: number;
  low_energy: number;
  avg_speed_kmh: number;
  partitions: number;
}

export interface LiveAlert {
  alert_id: string;
  vin: string;
  type: string;
  severity: string;
  title: string;
  ts_ms: number;
  detected_ms: number;
  lat: number | null;
  lon: number | null;
  details: Record<string, unknown>;
}

/** WebSocket stream of live KPIs and alerts. Authenticates with a one-time
 *  ticket, reconnects with back-off, keeps a short rolling history. */
export function useLiveStream(enabled = true) {
  const [kpi, setKpi] = useState<LiveKpi | null>(null);
  const [eps, setEps] = useState<{ t: number; eps: number }[]>([]);
  const [alerts, setAlerts] = useState<LiveAlert[]>([]);
  const [connected, setConnected] = useState(false);
  const retry = useRef(0);

  useEffect(() => {
    if (!enabled) return;
    let ws: WebSocket | null = null;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;

    const connect = async () => {
      try {
        const { ticket } = await api<{ ticket: string }>("/live/ws-ticket", { method: "POST" });
        const proto = location.protocol === "https:" ? "wss" : "ws";
        ws = new WebSocket(`${proto}://${location.host}/api/v1/live/stream?ticket=${encodeURIComponent(ticket)}`);
        ws.onopen = () => {
          setConnected(true);
          retry.current = 0;
        };
        ws.onmessage = (ev) => {
          const msg = JSON.parse(ev.data);
          if (msg.type === "kpi") {
            setKpi(msg.data);
            setEps((h) => [...h.slice(-179), { t: Date.now(), eps: msg.data.eps }]); // 3 min at 1 s pushes
          } else if (msg.type === "alert") {
            const incoming = msg.data as LiveAlert;
            // At-least-once delivery upstream: drop repeats of the same alert.
            setAlerts((a) => (a.some((x) => x.alert_id === incoming.alert_id) ? a : [incoming, ...a].slice(0, 50)));
          }
        };
        ws.onclose = () => {
          setConnected(false);
          if (!stopped) schedule();
        };
      } catch {
        schedule();
      }
    };
    const schedule = () => {
      retry.current = Math.min(retry.current + 1, 6);
      timer = setTimeout(connect, 500 * 2 ** retry.current);
    };
    connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      ws?.close();
    };
  }, [enabled]);

  return { kpi, eps, alerts, connected };
}
