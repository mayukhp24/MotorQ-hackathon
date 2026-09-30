# API latency per endpoint (k6, 100 req/s, 5 min)

| Endpoint | Requests | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---:|---:|---:|---:|
| `GET /api/v1/vehicles/{vin}` | 7,449 | 15.3 | 146.9 | 480.5 |
| `GET /api/v1/vehicles` | 6,046 | 28.8 | 159.3 | 429.6 |
| `GET /api/v1/alerts` | 4,527 | 16.8 | 119.2 | 353.0 |
| `GET /api/v1/fleet/summary` | 3,084 | 8.3 | 97.6 | 371.2 |
| `GET /api/v1/maintenance/risk` | 2,917 | 37.0 | 185.7 | 396.7 |
| `GET /api/v1/live/clusters` | 1,500 | 7.6 | 58.1 | 283.1 |
| `GET /api/v1/maintenance/summary` | 1,495 | 8.1 | 75.8 | 238.1 |
| `GET /api/v1/analytics/alert-trend` | 1,492 | 8.1 | 94.8 | 424.3 |
| `GET /api/v1/live/top-dtc` | 1,491 | 7.4 | 79.0 | 272.5 |
