# AI / ML components

FleetPulse uses two kinds of AI, each where rules fall short:

1. **A predictive-maintenance model** that ranks vehicles by the probability
   of a breakdown in the next 7 days, per failure mode, with reasons and a
   dollar impact.
2. **A maintenance copilot** (Claude with tool use) that answers fleet
   questions from live, tenant-scoped data and can *propose* (never execute)
   work orders.

## 1. Predictive maintenance

### Decision supported

"Which vehicles should go to the workshop this week?" A workshop can inspect
only a small share of the fleet per day. Rules ("any fault code in the last
7 days", "coolant ever above 104 °C") fire on thousands of healthy vehicles
and miss slow degradations that never cross a threshold: a coolant
temperature creeping up 9 °C over two weeks, a charging voltage sagging
0.5 V below its own baseline. The model ranks by trend against each vehicle's
own history, which rules cannot express compactly.

### Data and features

* **Source:** 60 days of synthetic telemetry for 100,000 vehicles
  (`services/pipeline/internal/sim`). Each vehicle has hidden component health
  states that degrade stochastically before a failure (cooling, 12 V battery /
  charging, ignition misfire, EV traction pack); the degradation shows up
  noisily in sensors and fault codes, together with benign noise and
  unrelated DTCs. Breakdowns are recorded as `service_event` rows.
* **Features (27):** built in ClickHouse from `vehicle_daily` per vehicle-day:
  3/7/14-day windows of coolant, 12 V voltage, pack temperature, their trends
  (3-day mean minus 14-day mean), fault-code counts by system over 3 and 7
  days, harsh events, km driven, idle ratio, days active, age, odometer,
  powertrain.
* **Label:** `1` if the vehicle breaks down in `(d, d + 7]` for that
  component (strictly future).
* **Leakage controls:** time-based split (train 15 Aug – 7 Sep, test
  8 – 22 Sep; no shuffling across time); features use only data up to day `d`;
  the last 8 days are never labelled; the breakdown day itself is excluded;
  a unit test pins the label window (`test_labels_look_forward_only`).

### Model design

* One `HistGradientBoostingClassifier` per component (handles missing sensor
  families, e.g. no coolant on EVs, and non-linear interactions), combined by
  **noisy-OR** into `risk_7d = 1 − Π(1 − p_c)`; the top component names the
  likely failure.
* Negatives down-sampled to 25% for training; probabilities are
  **prior-corrected** back to the true base rate, so `risk_7d` is a calibrated
  probability (top decile: predicted 14.8%, observed 15.0%).
* Physically impossible components are zeroed (EVs cannot misfire).
* **Explanations** are the top contributing features, rendered as sentences
  ("Coolant running +9.0 °C above its 14-day average").
* **Dollar impact:** `risk × (breakdown cost − planned repair cost)` per
  component; costs in `analytics/features.py`.

### Evaluation (held-out later period, 638,358 vehicle-days, base rate 2.0%)

Operating point: the workshop can inspect 1% of vehicle-days (k = 6,383);
every method gets the same budget.

| Method | PR-AUC | ROC-AUC | Precision @ k | Recall @ k | Net savings in test window |
|---|---:|---:|---:|---:|---:|
| **FleetPulse model** | **0.660** | **0.871** | **0.908** | **0.453** | **$8.28M** |
| Threshold alert rules (coolant ≥ 104 °C, battery < 11.8 V, pack ≥ 55 °C, ≥ 2 cooling/pack DTCs) | 0.211 | 0.777 | 0.411 | 0.205 | $4.31M |
| Any fault code in the last 7 days | 0.183 | 0.755 | 0.429 | 0.214 | $4.07M |

Lift over the rules: 3.1× PR-AUC, 2.2× precision at the same workshop
capacity, roughly double the avoided cost. Net savings = Σ over true positives
(unplanned breakdown cost − planned repair cost) − $80 per false alarm.

Per component PR-AUC: cooling 0.70, 12 V battery 0.70, EV pack 0.58,
misfire 0.46 (misfire has the weakest precursor signal in the data).

**Honesty note:** the data is synthetic. The simulator encodes plausible
physics and noise, but the absolute numbers say more about the method than
about any real fleet; the comparison against baselines on the same data is the
meaningful result. Validating on a pilot fleet's history is the first
next step.

### Operations

Weekly retraining (CronJob), daily scoring of all vehicles, metrics stored with
every `model_version` and exported to Prometheus; the active model is swapped
atomically in PostgreSQL.

## 2. Maintenance copilot

### Purpose

Managers ask questions that cut across screens ("which of my EVs are at risk
and do any already have work orders?"). The copilot turns that into tool
calls over the same APIs the UI uses, and can queue a work order for approval.

### Design

* **Model:** Claude via the Messages API with a manual tool-use loop
  (`services/fleet-api/app/agent/copilot.py`); model configurable
  (`LLM_MODEL`, default `claude-opus-5-5`), effort `medium`, server-side
  model fallbacks enabled, system prompt cached.
* **Tools (8):** `get_fleet_summary`, `list_at_risk_vehicles`,
  `get_vehicle_health`, `search_alerts`, `search_fault_knowledge`,
  `idle_cost_report`, `driver_safety_report`, `propose_work_order`. Strict
  JSON schemas; each tool maps to a permission and runs as the calling user
  under RLS, so the model cannot see more than the user can.
* **Retrieval:** a fault knowledge base (DTC guides and workshop case notes) in
  PostgreSQL `pgvector` with an HNSW cosine index; `search_fault_knowledge`
  embeds the question and returns the nearest documents.
* **Offline planner:** without an API key (or during an outage) a
  deterministic intent router calls the same tools and renders templated
  answers, so the feature degrades rather than disappears.

### Guardrails and cost

| Guardrail | Behaviour |
|---|---|
| Prompt-injection screen | Refuses before calling the model (`mode: guardrail`) |
| Input limit | 2,000 characters |
| Grounding check | VINs not present in the question or tool results are redacted |
| Tool budget | 6 tool calls per question, then the model must answer with what it has |
| Human in the loop | `propose_work_order` creates a pending action; only roles with `copilot:approve` can approve; one pending proposal per vehicle |
| Audit | Question, tools, arguments, proposals, token usage and latency in the audit chain |
| Cost | Token usage per answer recorded and priced at configurable list prices (`LLM_USD_PER_MTOK_IN/OUT`); Prometheus counters for tokens |

Latency: offline answers take 10–100 ms (measured). LLM mode was not exercised
in the build environment (no API key); its latency is dominated by model time
per tool round and is off every real-time path. Set `ANTHROPIC_API_KEY` to
enable it; the UI shows which mode answered.
