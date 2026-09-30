// FleetPulse API load test (k6).
//
//   k6 run tests/load/api.js                        # 5-minute load at 150 req/s
//   k6 run -e PROFILE=smoke tests/load/api.js       # CI smoke: 30 s at 20 req/s
//   k6 run -e PROFILE=soak  tests/load/api.js       # 2 h soak at 100 req/s
//   k6 run -e RATE=300 -e DURATION=2m tests/load/api.js   # ad-hoc override
//
// Traffic mirrors the dashboard: list/detail reads dominate, with live KPIs,
// alert feeds, risk rankings and analytics. Requests are spread across the 12
// demo users (3 tenants x 4 roles) so each stays under the per-user rate
// limit; the thresholds are the NFRs (p95 < 200 ms, p99 < 500 ms, < 1 % errors).
import http from "k6/http";
import { check, fail } from "k6";
import { Trend } from "k6/metrics";

const BASE = __ENV.BASE_URL || "http://localhost:8000";
const PASSWORD = __ENV.DEMO_PASSWORD || "FleetPulse!2026";
const PROFILE = __ENV.PROFILE || "load";

const profiles = {
  smoke: { rate: 20, duration: "30s", vus: 20 },
  load: { rate: 150, duration: "5m", vus: 150 },
  soak: { rate: 100, duration: "2h", vus: 100 },
};
const p = { ...profiles[PROFILE] };
if (__ENV.RATE) p.rate = Number(__ENV.RATE);
if (__ENV.DURATION) p.duration = __ENV.DURATION;
if (__ENV.RATE) p.vus = Math.max(20, p.rate);

export const options = {
  scenarios: {
    dashboard: {
      executor: "constant-arrival-rate",
      rate: p.rate,
      timeUnit: "1s",
      duration: p.duration,
      preAllocatedVUs: p.vus,
      maxVUs: p.vus * 3,
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: ["p(95)<200", "p(99)<500"],
    "http_req_duration{kind:list}": ["p(95)<200"],
    "http_req_duration{kind:detail}": ["p(95)<200"],
  },
  summaryTrendStats: ["avg", "min", "med", "p(90)", "p(95)", "p(99)", "max"],
};

const tenants = ["acme", "rapidride", "greenfleet"];
const roles = ["admin", "maint", "analyst", "viewer"];
const pageSize = new Trend("vehicle_page_size");

export function setup() {
  const sessions = [];
  for (const t of tenants) {
    for (const r of roles) {
      const res = http.post(`${BASE}/api/v1/auth/token`, { username: `${r}@${t}.demo`, password: PASSWORD, grant_type: "password" });
      if (res.status !== 200) fail(`login ${r}@${t}.demo -> ${res.status}`);
      const token = res.json("access_token");
      const params = { headers: { Authorization: `Bearer ${token}` } };
      const list = http.get(`${BASE}/api/v1/vehicles?limit=200`, params);
      const vins = list.json("items").map((v) => v.vin);
      sessions.push({ role: r, token, vins, cursor: list.json("next_cursor") });
    }
  }
  return { sessions };
}

function pick(a) {
  return a[Math.floor(Math.random() * a.length)];
}

// Weighted mix of dashboard calls (weights sum to 100). `staff` calls need a
// role with maintenance/analytics permission (viewers get 403 by design).
const mix = [
  [25, "detail", false, (s) => `/vehicles/${pick(s.vins)}`],
  [20, "list", false, (s) => `/vehicles?limit=50${Math.random() < 0.5 && s.cursor ? `&cursor=${encodeURIComponent(s.cursor)}` : ""}`],
  [15, "list", false, () => `/alerts?limit=50&status=OPEN`],
  [10, "live", false, () => `/fleet/summary`],
  [10, "list", true, () => `/maintenance/risk?limit=50`],
  [5, "live", false, () => `/live/clusters?precision=4`],
  [5, "live", false, () => `/live/top-dtc`],
  [5, "analytics", true, () => `/maintenance/summary`],
  [5, "analytics", true, () => `/analytics/alert-trend?days=14`],
];

export default function (data) {
  let r = Math.random() * 100;
  let chosen = mix[0];
  for (const m of mix) {
    if ((r -= m[0]) < 0) {
      chosen = m;
      break;
    }
  }
  const [, kind, staff, route] = chosen;
  const s = pick(staff ? data.sessions.filter((x) => x.role !== "viewer") : data.sessions);
  const path = route(s);
  const res = http.get(`${BASE}/api/v1${path}`, {
    headers: { Authorization: `Bearer ${s.token}` },
    tags: { kind, name: path.split("?")[0].replace(/\/[A-HJ-NPR-Z0-9]{17}$/, "/{vin}") },
  });
  check(res, { "status 200": (x) => x.status === 200 });
  if (path.startsWith("/vehicles?") && res.status === 200) pageSize.add(res.json("items").length);
}
