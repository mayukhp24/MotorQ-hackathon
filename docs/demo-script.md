# Demo video plan (≤ 5:00)

Recording setup: stack running with `docker compose up -d`, simulator at the
default 5 s interval, browser at 1440×900, Grafana in a second tab. Sign in as
`maint@acme.demo` (password `FleetPulse!2026`) unless noted.

| Time | Segment | What to show |
|---|---|---|
| 0:00 – 0:30 | Problem | A roadside breakdown costs a fleet ~$2,400 (tow, repair, lost day) against ~$520 if caught in the workshop, and today's rules either flood managers with fault codes or miss slow degradations. Three fleets, 100,000 vehicles, three incompatible OEM clouds. |
| 0:30 – 1:00 | Solution | One line: "FleetPulse turns raw OEM telemetry into the list of vehicles to pull in this week, with the reason and the dollar impact, plus real-time critical alerts." Show the Overview: vehicles online, events/s, open critical alerts, predicted breakdowns, savings. |
| 1:00 – 1:40 | Real-time path | Run `scripts/send-sample.py` and the BDD overheat scenario (`behave features/realtime_alerts.feature`); the ENGINE_OVERHEAT alert appears in *Live alerts* within a second. Open *Live map* (clusters by geohash, coarse positions for analysts). |
| 1:40 – 2:40 | Predictive maintenance | *Predictive maintenance*: ranking with reasons and $; open the top vehicle (risk, "why the model thinks so", telemetry trend); *Schedule* on a ranked row creates a work order. Mention model vs rules: precision 0.91 vs 0.41 at the same workshop capacity. |
| 2:40 – 3:20 | Copilot | "Which vehicles are most likely to break down this week?" then "Schedule a repair for the riskiest one" → proposal appears under *Pending approvals*; approve it. Sign in as a viewer: approval is forbidden. Try a prompt injection: refused. |
| 3:20 – 3:50 | Trust | Sign in as `analyst@acme.demo`: positions snapped to a 5 km grid, drivers pseudonymised. `admin@greenfleet.demo` cannot open an Acme vehicle (404). *Compliance*: audit trail, *Verify integrity*, erase a driver and show the report. |
| 3:50 – 4:30 | Scale and resilience | Grafana: ingest/processed events/s, consumer lag, p95 alert latency. Kill a stream processor (`docker kill`) and watch lag recover; show the chaos report and the k6 result (100 req/s, p95 135 ms, 0 errors). |
| 4:30 – 5:00 | Architecture and close | One slide: architecture diagram, stores and why (Kafka, ClickHouse, PostgreSQL + RLS + pgvector, Redis); `helm install` / Terraform for AWS; what is next (pilot data, per-tenant quotas, audit partitioning). |

Feature timestamps for the feature table (F-xx) come from this plan; update
them after recording.
