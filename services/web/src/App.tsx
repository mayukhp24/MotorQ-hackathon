import { lazy, Suspense, type ReactNode } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import Layout from "./components/Layout";
import { Spinner } from "./components/ui";
import { useAuth } from "./lib/auth";
import Login from "./pages/Login";

const Overview = lazy(() => import("./pages/Overview"));
const LiveMap = lazy(() => import("./pages/LiveMap"));
const Vehicles = lazy(() => import("./pages/Vehicles"));
const VehicleDetail = lazy(() => import("./pages/VehicleDetail"));
const Alerts = lazy(() => import("./pages/Alerts"));
const Maintenance = lazy(() => import("./pages/Maintenance"));
const Analytics = lazy(() => import("./pages/Analytics"));
const Copilot = lazy(() => import("./pages/Copilot"));
const Compliance = lazy(() => import("./pages/Compliance"));
const Platform = lazy(() => import("./pages/Platform"));

function Guard({ perm, children }: { perm: string; children: ReactNode }) {
  const { can } = useAuth();
  return can(perm) ? <>{children}</> : <div className="card p-8 text-sm text-ink-2">You don't have access to this page ({perm}).</div>;
}

export default function App() {
  const { me, loading } = useAuth();
  if (loading) return <Spinner label="Signing in" />;
  if (!me) return <Routes><Route path="*" element={<Login />} /></Routes>;
  const home = me.tenant_id ? <Overview /> : <Navigate to="/platform" replace />;
  return (
    <Suspense fallback={<Spinner />}>
      <Routes>
        <Route path="/login" element={<Navigate to="/" replace />} />
        <Route element={<Layout />}>
          <Route index element={home} />
          <Route path="map" element={<Guard perm="fleet:read"><LiveMap /></Guard>} />
          <Route path="vehicles" element={<Guard perm="vehicle:read"><Vehicles /></Guard>} />
          <Route path="vehicles/:vin" element={<Guard perm="vehicle:read"><VehicleDetail /></Guard>} />
          <Route path="alerts" element={<Guard perm="alert:read"><Alerts /></Guard>} />
          <Route path="maintenance" element={<Guard perm="maintenance:read"><Maintenance /></Guard>} />
          <Route path="analytics" element={<Guard perm="analytics:read"><Analytics /></Guard>} />
          <Route path="copilot" element={<Guard perm="copilot:use"><Copilot /></Guard>} />
          <Route path="compliance" element={<Guard perm="audit:read"><Compliance /></Guard>} />
          <Route path="platform" element={<Guard perm="platform:admin"><Platform /></Guard>} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </Suspense>
  );
}
