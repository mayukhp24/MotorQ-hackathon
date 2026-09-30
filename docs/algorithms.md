# Algorithms and data structures

Every structure on the hot path has bounded memory and O(1) or O(log n) work
per event. Measured costs come from `docs/evidence/pipeline-benchmarks.txt`
(single core, Intel Xeon 2.8 GHz).

| Problem | Algorithm / structure | Time | Space | Measured | Code |
|---|---|---|---|---|---|
| Reject corrupted vehicle IDs at the edge | VIN check digit (ISO 3779 transliteration + weights mod 11) | O(17) | O(1) | 319 ns/VIN | `internal/vin/vin.go` |
| Normalise fault codes from 3 OEM spellings | Two regexes: loose `(?i)\b([PCBU])[-_ ]?([0-3][0-9A-F]{3})\b` → canonical `^[PCBU][0-3][0-9A-F]{3}$` | O(len) | O(1) | part of decode | `internal/dtc/dtc.go` |
| Map 3 payload formats to one event | Declarative field mapping (path, unit conversion, time format) compiled once per OEM | O(fields) per record | O(spec) | ~10 µs/record incl. validation | `internal/oem/` |
| Drop OEM retries / replays | Time-rotating pair of Bloom filters | O(k) | O(n) bits, bounded | 275–424 ns/event | `internal/stream/bloom.go` |
| Trending fault codes (top-K) | Count-Min Sketch + min-heap of K | O(d + log K) | O(w·d + K) | — | `internal/stream/countmin.go` |
| Vehicle density map | Geohash (interleaved lat/lon bits, base32) at precision 3–5 | O(precision) | O(1) | 123–148 ns | `internal/stream/geohash.go` |
| Sustained-condition faults | Per-vehicle hold timer in event time + per-rule cooldown | O(1) per event per rule | O(vehicles × rules) | part of ~8 µs/event | `internal/detect/detect.go` |
| Trips, idling | State machine PARKED → DRIVING/IDLING → PARKED | O(1) | O(vehicles) | part of ~8 µs/event | `internal/detect/detect.go` |
| Paging through 100K rows | Keyset pagination with HMAC-signed cursor | O(log n + page) | O(1) | 1.2 ms at page 1,000 | `app/domain/pagination.py` |
| 7-day breakdown risk | Gradient-boosted trees per component + noisy-OR | O(trees × depth) per vehicle | O(model) | 100K vehicles scored in one job | `analytics/model.py` |
| Knowledge search for the copilot | Signed feature hashing (256-d) + HNSW cosine index (pgvector) | O(tokens) embed, ~O(log n) search | O(n·d) | — | `analytics/embeddings.py` |

## 1. Duplicate suppression: rotating Bloom filters

OEM clouds retry on timeouts, so the same reading can arrive two or three
times, minutes apart and on either transport. Exact dedup would need every
event ID in memory; at 100K events/s that grows without bound.

```
NewBloom(n, p):  m = ceil(-n ln p / (ln 2)^2) bits;  k = round((m/n) ln 2)
index_i(key) = (h1(key) + i * h2(key)) mod m       -- Kirsch–Mitzenmacher, 2 hashes → k indexes

Dedup.Seen(key):                                     -- two generations: current, previous
    if now - rotatedAt > period: previous = current; current = empty; rotatedAt = now
    if current.Test(key) or previous.Test(key): return true
    current.Add(key); return false
```

* A key is remembered for at least one period and at most two (2–4 minutes
  with the default 2-minute period), which covers OEM retry windows.
* Per partition: n = 2,000,000 keys/period, p = 0.001 → m ≈ 28.8M bits
  (3.6 MB), k = 10; two generations = 7.2 MB per partition, independent of
  stream length.
* False positives (0.1%) drop a genuine event; a false negative is impossible.
  Downstream sinks are idempotent anyway (deterministic event and alert IDs,
  ReplacingMergeTree), so the filter is an efficiency layer, not the only
  line of defence.

## 2. Trending fault codes: Count-Min Sketch + heap

"Which fault codes are spiking across the fleet right now?" needs per-code
counts over a 15-minute window across thousands of distinct codes and
vehicles.

```
w = ceil(e / eps), d = ceil(ln(1 / delta))
Add(key):   for row r in 0..d-1: C[r][h_r(key) mod w] += 1
            est = min_r C[r][h_r(key) mod w]
            heap.offer(key, est)         -- min-heap of size K keeps the heavy hitters
Estimate never under-counts; over-count ≤ eps · N with probability 1 - delta.
```

Each processor keeps one sketch per tenant; the API merges the per-processor
top-K lists from Redis.

## 3. Sustained-condition rules

Single readings are noisy; an alert must represent a real condition, and a
flapping sensor must not page a manager every second.

```
on event e for vehicle v, rule R (e.g. overheat: coolant ≥ 110 °C for 15 s):
    if R.condition(e):
        if v.since[R] is unset: v.since[R] = e.ts             -- event time, not arrival time
        if e.ts - v.since[R] ≥ R.hold and e.ts - v.lastFired[R] ≥ cooldown(15 min):
            emit alert(id = uuid5(vin | R | cooldown bucket))  -- deterministic: replays and
            v.lastFired[R] = e.ts                              -- duplicates produce the same ID
    else:
        unset v.since[R]
late events (ts < v.lastTs - tolerance) update history only, never rules or live state
```

Twelve rules: critical/major DTC, engine overheat, coolant high, low 12 V
voltage, EV pack over-temperature, low state of charge, crash (OEM crash
flag), risky driving (≥ 3 harsh events in 5 min, where harsh = OEM flag or
derived acceleration ≤ −6.5 / ≥ 4.5 m/s²), excessive idle (10 min),
overspeed (120 km/h for 30 s), low tyre pressure. Thresholds:
`detect.DefaultThresholds()`.

## 4. Trip segmentation and idle cost

```
status(e) = CHARGING if charging
          = DRIVING  if ignition and speed ≥ 3 km/h
          = IDLING   if ignition
          = PARKED   otherwise
trip:   opens on ignition with speed ≥ 5 km/h (a vehicle idling in the yard is not a trip)
        while open, per sample with 0 < dt ≤ 60 s:
            idle += dt if speed < 1 km/h;  overspeed += dt if speed > 120 km/h
            count harsh brakes / accelerations; track max speed and energy used
        closes on ignition off: distance = odometer_end − odometer_start
                                emit trip(id = uuid5(vin | start)) if distance ≥ 0.2 km
derived acceleration (when the OEM sends none) only across gaps ≤ 3 s
idle cost = idle hours × 0.9 L/h × fuel price   (EVs: 2 kW × power price)
```

Trips carry harsh events, overspeed seconds and idle seconds, which feed the
driver-safety score and the idle-cost report.

## 5. Keyset pagination

```
page(cursor, limit):
    (last_key) = verify_hmac(cursor)                       -- tamper-proof, opaque to clients
    rows = SELECT … WHERE tenant_id = $t AND sort_key > last_key ORDER BY sort_key LIMIT limit + 1
    next = sign(rows[limit - 1].sort_key) if len(rows) > limit else null
```

O(log n) index seek + O(limit) rows at any depth, versus O(offset + limit) for
`OFFSET`, and no skipped or repeated rows when data changes between pages.

## 6. Breakdown risk: per-component boosting + noisy-OR

```
for component c in {cooling, battery12v, misfire, ev_pack}:
    p_c = HistGradientBoosting_c(features)                  -- trained on down-sampled negatives
    p_c = p_c · r / (p_c · r + 1 - p_c)                     -- prior correction, r = negative sample rate
    p_c = 0 if component impossible for powertrain          -- EVs cannot misfire
risk_7d = 1 - Π_c (1 - p_c)                                 -- noisy-OR: any component fails
reasons = top feature contributions for argmax_c p_c, rendered as sentences
```

Training is time-split (train on earlier days, evaluate on later days; no
shuffling across time), labels look strictly forward
(`breakdown within (d, d + 7]`). Evaluation and baselines: `docs/ai-ml.md`.

## 7. Knowledge embeddings: signed feature hashing

```
embed(text): v = zeros(256)
    for token in unigrams + bigrams(text):
        h = blake2b(token); v[h mod 256] += (+1 if h.bit else -1)
    return v / ||v||
search: ORDER BY embedding <=> embed(query) LIMIT k            -- HNSW, cosine
```

Deterministic, dependency-free and identical in the API and the batch job, so
the knowledge base can be rebuilt without an external embedding service. A
learned embedding model is a drop-in upgrade (same column, same index).

## Scale tested

| Input | Result |
|---|---|
| 100,000 vehicles, 60 days of history (74M telemetry rows, 2M trips, 536K alerts) | seeded in ~85 s; all queries above measured on it |
| 30,000 API requests at 100 req/s | p95 135 ms, p99 408 ms, 0 errors |
| ~4,000 events/s for > 1 h with bursts, duplicates, out-of-order and outages | consumer lag 0 at steady state; duplicates filtered (processor `duplicate` counter) |
| Single-core benchmarks | processor 116–134K events/s, gateway ~100K records/s |
