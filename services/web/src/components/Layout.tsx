import clsx from "clsx";
import {
  Activity, BarChart3, Bell, Bot, Car, LayoutDashboard, LogOut, Map as MapIcon, Menu, Monitor, Moon, ServerCog,
  ShieldCheck, Sun, Wrench, X,
} from "lucide-react";
import { createContext, useContext, useState, type ReactNode } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { useAuth } from "../lib/auth";
import ErrorBoundary from "./ErrorBoundary";
import { useLiveStream } from "../lib/live";
import { useTheme } from "../lib/theme";

type Live = ReturnType<typeof useLiveStream>;
const LiveCtx = createContext<Live | null>(null);
export const useLive = () => useContext(LiveCtx)!;

const NAV = [
  { to: "/", label: "Overview", icon: LayoutDashboard, perm: "fleet:read" },
  { to: "/map", label: "Live map", icon: MapIcon, perm: "fleet:read" },
  { to: "/vehicles", label: "Vehicles", icon: Car, perm: "vehicle:read" },
  { to: "/alerts", label: "Alerts", icon: Bell, perm: "alert:read" },
  { to: "/maintenance", label: "Predictive maintenance", icon: Wrench, perm: "maintenance:read" },
  { to: "/analytics", label: "Cost & safety", icon: BarChart3, perm: "analytics:read" },
  { to: "/copilot", label: "Copilot", icon: Bot, perm: "copilot:use" },
  { to: "/compliance", label: "Compliance", icon: ShieldCheck, perm: "audit:read" },
  { to: "/platform", label: "Platform", icon: ServerCog, perm: "platform:admin" },
];

const ROLE_LABEL: Record<string, string> = {
  platform_admin: "Platform operator", fleet_admin: "Fleet admin", maintenance_manager: "Maintenance manager",
  analyst: "Analyst", viewer: "Viewer",
};

function ThemeToggle() {
  const { mode, setMode } = useTheme();
  const next = mode === "system" ? "light" : mode === "light" ? "dark" : "system";
  const Icon = mode === "system" ? Monitor : mode === "light" ? Sun : Moon;
  return (
    <button className="btn-ghost px-2" onClick={() => setMode(next)} title={`Theme: ${mode} (click for ${next})`} aria-label="Toggle theme">
      <Icon className="h-4 w-4" />
    </button>
  );
}

function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const { can } = useAuth();
  return (
    <nav className="flex h-full flex-col gap-1 p-3">
      <div className="mb-4 flex items-center gap-2 px-2 pt-1">
        <div className="grid h-8 w-8 place-items-center rounded-lg bg-accent text-white">
          <Activity className="h-4 w-4" />
        </div>
        <div>
          <div className="text-sm font-semibold leading-tight text-ink-1">FleetPulse</div>
          <div className="text-[11px] leading-tight text-ink-3">Fleet health intelligence</div>
        </div>
      </div>
      {NAV.filter((n) => can(n.perm)).map((n) => (
        <NavLink
          key={n.to}
          to={n.to}
          end={n.to === "/"}
          onClick={onNavigate}
          className={({ isActive }) =>
            clsx(
              "flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors",
              isActive ? "bg-accent-soft font-medium text-accent-ink" : "text-ink-2 hover:bg-surface-2 hover:text-ink-1",
            )
          }
        >
          <n.icon className="h-4 w-4" aria-hidden />
          {n.label}
        </NavLink>
      ))}
    </nav>
  );
}

export default function Layout() {
  const { me, logout, can } = useAuth();
  const live = useLiveStream(can("fleet:read"));
  const [open, setOpen] = useState(false);
  const { pathname } = useLocation();
  return (
    <LiveCtx.Provider value={live}>
      <div className="flex h-full">
        <aside className="hidden w-60 shrink-0 border-r border-line bg-surface-3 lg:block">
          <Sidebar />
        </aside>
        {open && (
          <div className="fixed inset-0 z-[1000] flex lg:hidden">
            <div className="w-64 border-r border-line bg-surface-3">
              <div className="flex justify-end p-2">
                <button className="btn-ghost px-2" onClick={() => setOpen(false)} aria-label="Close menu"><X className="h-4 w-4" /></button>
              </div>
              <Sidebar onNavigate={() => setOpen(false)} />
            </div>
            <div className="flex-1 bg-black/40" onClick={() => setOpen(false)} />
          </div>
        )}
        <div className="flex min-w-0 flex-1 flex-col">
          <header className="sticky top-0 z-[500] flex h-14 items-center gap-3 border-b border-line bg-surface-1 px-4">
            <button className="btn-ghost px-2 lg:hidden" onClick={() => setOpen(true)} aria-label="Open menu"><Menu className="h-4 w-4" /></button>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-semibold text-ink-1">{me?.tenant_name ?? "Platform"}</div>
            </div>
            <LiveIndicator connected={live.connected} eps={live.kpi?.eps} />
            <ThemeToggle />
            <div className="hidden text-right sm:block">
              <div className="text-sm leading-tight text-ink-1">{me?.name}</div>
              <div className="text-[11px] leading-tight text-ink-3">{me?.roles.map((r) => ROLE_LABEL[r] ?? r).join(", ")}</div>
            </div>
            <button className="btn-ghost px-2" onClick={logout} title="Sign out" aria-label="Sign out"><LogOut className="h-4 w-4" /></button>
          </header>
          <main className="min-w-0 flex-1 overflow-y-auto px-4 py-6 sm:px-6">
            <div className="mx-auto max-w-[1400px]"><ErrorBoundary key={pathname}><Outlet /></ErrorBoundary></div>
          </main>
        </div>
      </div>
    </LiveCtx.Provider>
  );
}

function LiveIndicator({ connected, eps }: { connected: boolean; eps?: number }) {
  return (
    <div className="hidden items-center gap-2 rounded-full border border-line bg-surface-2 px-2.5 py-1 text-xs text-ink-2 md:flex"
      title={connected ? "Live stream connected" : "Reconnecting…"}>
      <span className={clsx("h-2 w-2 rounded-full", connected && "live-dot")}
        style={{ background: connected ? "var(--status-good)" : "var(--text-muted)" }} />
      {connected ? (eps != null ? `${Math.round(eps).toLocaleString()} events/s` : "Live") : "Offline"}
    </div>
  );
}

export function Section({ children }: { children: ReactNode }) {
  return <div className="grid gap-4">{children}</div>;
}
