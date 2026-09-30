import { Activity, Lock } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Navigate } from "react-router-dom";
import { useAuth } from "../lib/auth";

const DEMO = [
  { email: "maint@acme.demo", role: "Maintenance manager", tenant: "Acme Logistics" },
  { email: "admin@acme.demo", role: "Fleet admin", tenant: "Acme Logistics" },
  { email: "analyst@acme.demo", role: "Analyst (masked data)", tenant: "Acme Logistics" },
  { email: "admin@greenfleet.demo", role: "Fleet admin", tenant: "GreenFleet EV" },
  { email: "ops@fleetpulse.dev", role: "Platform operator", tenant: "All tenants" },
];

export default function Login() {
  const { me, login } = useAuth();
  const [email, setEmail] = useState("maint@acme.demo");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  if (me) return <Navigate to={me.tenant_id ? "/" : "/platform"} replace />;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(email, password);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Login failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-full lg:grid-cols-2">
      <div className="relative hidden overflow-hidden bg-[#0d1b2e] p-12 text-white lg:flex lg:flex-col lg:justify-between">
        <div className="flex items-center gap-2">
          <div className="grid h-9 w-9 place-items-center rounded-lg bg-[#3987e5]"><Activity className="h-5 w-5" /></div>
          <span className="text-lg font-semibold">FleetPulse</span>
        </div>
        <div className="max-w-md">
          <h1 className="text-3xl font-semibold leading-tight">Know which vehicles will break down — before they do.</h1>
          <p className="mt-4 text-[15px] leading-relaxed text-white/75">
            Real-time telemetry from 100,000 connected vehicles across three OEM clouds, turned into 7-day breakdown
            predictions, live fault alerts and the dollar impact of acting now.
          </p>
          <dl className="mt-8 grid grid-cols-3 gap-4 text-sm">
            {[["100K", "vehicles streaming"], ["3 OEMs", "normalised live"], ["< 2 s", "event to dashboard"]].map(([v, l]) => (
              <div key={l}><dt className="text-2xl font-semibold">{v}</dt><dd className="text-white/60">{l}</dd></div>
            ))}
          </dl>
        </div>
        <p className="text-xs text-white/40">Synthetic data only · Hackathon build</p>
        <svg className="pointer-events-none absolute -right-24 top-24 h-[520px] w-[520px] opacity-20" viewBox="0 0 200 200" aria-hidden>
          {Array.from({ length: 9 }).map((_, i) => (
            <circle key={i} cx="100" cy="100" r={12 + i * 11} fill="none" stroke="#3987e5" strokeWidth="0.6" />
          ))}
        </svg>
      </div>
      <div className="flex items-center justify-center p-6">
        <form onSubmit={submit} className="w-full max-w-sm">
          <h2 className="text-xl font-semibold text-ink-1">Sign in</h2>
          <p className="mt-1 text-sm text-ink-2">Use a demo account below. The password is in the README.</p>
          <label className="mt-6 block text-sm font-medium text-ink-1" htmlFor="email">Email</label>
          <input id="email" className="input mt-1" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="username" />
          <label className="mt-4 block text-sm font-medium text-ink-1" htmlFor="pw">Password</label>
          <input id="pw" type="password" className="input mt-1" value={password} onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password" required />
          {error && <p className="mt-3 text-sm" style={{ color: "var(--status-critical)" }} role="alert">{error}</p>}
          <button className="btn-primary mt-5 w-full justify-center py-2" disabled={busy}>
            <Lock className="h-4 w-4" /> {busy ? "Signing in…" : "Sign in"}
          </button>
          <div className="mt-8">
            <div className="label mb-2">Demo accounts</div>
            <div className="grid gap-1.5">
              {DEMO.map((d) => (
                <button type="button" key={d.email} onClick={() => setEmail(d.email)}
                  className="flex items-center justify-between rounded-lg border border-line px-3 py-2 text-left text-sm hover:bg-surface-2">
                  <span className="font-medium text-ink-1">{d.role}</span>
                  <span className="text-xs text-ink-3">{d.tenant}</span>
                </button>
              ))}
            </div>
          </div>
        </form>
      </div>
    </div>
  );
}
