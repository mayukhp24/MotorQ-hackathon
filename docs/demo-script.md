# Demo video plan (≤ 5:00)

Segments follow the solution-document template; timestamps match the feature
table (F-xx) in the solution document. Re-time after recording.

Setup: stack running (`docker compose up -d`), simulator at the default 5 s
interval, browser at 1440×900, Grafana in a second tab, a terminal in the repo.
Sign in as `maint@acme.demo` (password `FleetPulse!2026`) unless noted.

| Time | Segment | What to show |
|---|---|---|
| 0:00 – 0:30 | Problem | A roadside breakdown costs ~$2,400 against ~$520 in the workshop; rules flood managers with fault codes or miss slow failures; three fleets, 100,000 vehicles, three incompatible OEM clouds. |
| 0:30 – 1:00 | Solution | One line: "the list of vehicles to pull in this week, with the reason and the saving, plus sub-second critical alerts". |
| 1:00 | Live demo | **F-04** Overview: vehicles online, events/s, open critical alerts, predicted breakdowns, savings. |
| 1:05 | | **F-01** `scripts/send-sample.py`: three OEM formats accepted. |
| 1:10 | | **F-02** `scripts/send-sample.py pinnacle --corrupt`: 1 accepted, 1 rejected to the DLQ. |
| 1:15 | | **F-03** `cd tests/bdd && behave features/realtime_alerts.feature`: ENGINE_OVERHEAT appears in *Live alerts* within a second. |
| 1:25 | | **F-05** Live map: density clusters, vehicle positions. |
| 1:40 | | **F-06** Vehicle detail: live state, telemetry, risk with reasons. |
| 1:50 | | **F-07** Predictive maintenance: ranking with component, reasons and $; precision 0.91 vs 0.41 for rules. |
| 2:05 | | **F-08** *Schedule* on a ranked row → work order. |
| 2:15 | | **F-09** Copilot: "Which vehicles are most likely to break down this week?", then "Schedule a repair for the riskiest one" → approve under *Pending approvals*; a prompt injection is refused. |
| 2:35 | | **F-10** Cost & safety: idle cost and driver safety. |
| 2:45 | | **F-11** Sign in as `analyst@acme.demo` (coarse positions, pseudonymised drivers); `admin@greenfleet.demo` gets 404 on an Acme vehicle. |
| 2:50 | | **F-12** Compliance: audit trail, *Verify integrity*, erase a driver and show the report. |
| 3:00 | Under the hood | **F-13** Architecture diagram (`docs/diagrams/02-containers.png`); the simulator's bursts, duplicates and outages. |
| 3:30 | | **F-14** Grafana: ingest vs processed events/s, consumer lag, p95 alert latency, API latency. |
| 3:45 | | **F-15** `docker kill motorq-hackathon-stream-processor-1`; lag recovers; show `docs/evidence/chaos-report.md` and the k6 result (100 req/s, p95 135 ms, 0 errors). |
| 4:05 | | **F-17** Helm chart and Terraform (AWS, GCP values); one EXPLAIN before/after (351 ms → 1.3 ms). |
| 4:15 – 5:00 | Impact & next steps | Precision 0.91 vs 0.41 and $8.3M vs $4.3M in the back-test; alert p95 480 ms; API p95 135 ms; pilot plan (real feeds, cloud load test, SSO); the team. |
