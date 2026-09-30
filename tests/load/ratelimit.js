// Verifies the per-user rate limit engages (429 + Retry-After) and that a
// throttled user does not affect another tenant's latency.
//   k6 run tests/load/ratelimit.js
import http from "k6/http";
import { check } from "k6";
import { Counter } from "k6/metrics";

const BASE = __ENV.BASE_URL || "http://localhost:8000";
const PASSWORD = __ENV.DEMO_PASSWORD || "FleetPulse!2026";
const limited = new Counter("throttled_responses");

export const options = {
  scenarios: {
    noisy: { executor: "constant-arrival-rate", rate: 60, timeUnit: "1s", duration: "40s", preAllocatedVUs: 60, exec: "noisy" },
    neighbour: { executor: "constant-arrival-rate", rate: 5, timeUnit: "1s", duration: "40s", preAllocatedVUs: 10, exec: "neighbour" },
  },
  thresholds: {
    throttled_responses: ["count>0"],
    "http_req_duration{scenario:neighbour}": ["p(95)<200"],
    "checks{scenario:neighbour}": ["rate>0.99"],
  },
};

function token(email) {
  return http.post(`${BASE}/api/v1/auth/token`, { username: email, password: PASSWORD, grant_type: "password" }).json("access_token");
}

export function setup() {
  return { noisy: token("viewer@rapidride.demo"), quiet: token("viewer@greenfleet.demo") };
}

export function noisy(d) {
  const res = http.get(`${BASE}/api/v1/fleet/summary`, { headers: { Authorization: `Bearer ${d.noisy}` } });
  if (res.status === 429) {
    limited.add(1);
    check(res, { "429 carries Retry-After": (r) => Number(r.headers["Retry-After"]) > 0 });
  }
}

export function neighbour(d) {
  const res = http.get(`${BASE}/api/v1/fleet/summary`, { headers: { Authorization: `Bearer ${d.quiet}` } });
  check(res, { "neighbour unaffected": (r) => r.status === 200 });
}
