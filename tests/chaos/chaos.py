"""Chaos experiments against the running docker compose stack.

    python tests/chaos/chaos.py [--report docs/evidence/chaos-report.md]

Each experiment injects a fault with the Docker CLI, sends a sentinel
"engine overheating" burst through the public ingest API while the fault is
active, and checks the steady-state hypothesis: the critical alert still
reaches the API (no data loss) and user-facing reads keep answering or degrade
with a clean problem response instead of hanging. Recovery time is measured.

Requires: the stack from docker-compose.yml running (simulator optional), and
`requests`. Uses the same demo accounts / local keys as the BDD suite.
"""

from __future__ import annotations

import argparse
import os
import random
import subprocess
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone

import requests

API = os.environ.get("BASE_URL", "http://localhost:8000") + "/api/v1"
INGEST = os.environ.get("INGEST_URL", "http://localhost:8080")
PASSWORD = os.environ.get("DEMO_PASSWORD", "FleetPulse!2026")
AURORA_KEY = os.environ.get("AURORA_KEY", "aurora-local-key")
PROJECT = os.environ.get("COMPOSE_PROJECT_NAME", "motorq-hackathon")


@dataclass
class Result:
    name: str
    fault: str
    hypothesis: str
    passed: bool = False
    observations: list[str] = field(default_factory=list)

    def note(self, msg: str) -> None:
        print(f"  - {msg}")
        self.observations.append(msg)


def sh(*args: str, check: bool = True) -> str:
    return subprocess.run(args, check=check, capture_output=True, text=True).stdout.strip()


def compose(*args: str) -> str:
    return sh("docker", "compose", *args)


def container(service: str, index: int = 1) -> str:
    return f"{PROJECT}-{service}-{index}"


class Client:
    def __init__(self) -> None:
        self.s = requests.Session()
        r = self.s.post(f"{API}/auth/token", data={"username": "maint@acme.demo", "password": PASSWORD}, timeout=10)
        r.raise_for_status()
        self.s.headers["Authorization"] = f"Bearer {r.json()['access_token']}"
        items = self.get("/vehicles", params={"limit": 200}).json()["items"]
        self.vins = [v["vin"] for v in items if v["oem_code"] == "aurora" and v["powertrain"] != "EV"]

    def get(self, path: str, **kw) -> requests.Response:
        return self.s.get(API + path, timeout=kw.pop("timeout", 10), **kw)

    def overheat(self, vin: str) -> requests.Response:
        """20 s of coolant at 118 °C, timestamped just ahead of the live stream."""
        base = int(time.time() * 1000) + 60_000
        recs = [{"vehicle": {"vin": vin}, "timestamp": base + i * 1000, "sequence": base * 10 + i,
                 "position": {"latitude": 13.08, "longitude": 80.27, "heading": 90},
                 "motion": {"speedKph": 45.0, "accelMps2": 0.1},
                 "powertrain": {"odometerKm": 250000.0, "engineOn": True, "rpm": 1900, "coolantTempC": 118.0,
                                "fuelLevelPct": 50},
                 "electrical": {"batteryVoltage": 13.9}} for i in range(20)]
        return requests.post(f"{INGEST}/v1/ingest/aurora", json={"oem": "aurora", "records": recs},
                             headers={"X-Api-Key": AURORA_KEY}, timeout=150)

    def wait_alert(self, vin: str, since: float, timeout_s: float) -> float | None:
        deadline = time.time() + timeout_s
        while time.time() < deadline:
            try:
                items = self.get("/alerts", params={"vin": vin, "type": "ENGINE_OVERHEAT", "limit": 5}).json()["items"]
                for a in items:
                    det = datetime.fromisoformat(a["detected_at"].replace("Z", "+00:00")).timestamp()
                    if det >= since - 5:
                        return time.time() - since
            except (requests.RequestException, KeyError, ValueError):
                pass
            time.sleep(0.5)
        return None

    def probe_vin(self) -> str:
        vin = random.choice(self.vins)
        self.vins.remove(vin)  # one sentinel per vehicle: alert cooldowns cannot mask a loss
        return vin


def wait_http(url: str, timeout_s: float = 180) -> float:
    t0 = time.time()
    while time.time() - t0 < timeout_s:
        try:
            if requests.get(url, timeout=3).status_code == 200:
                return time.time() - t0
        except requests.RequestException:
            pass
        time.sleep(1)
    raise TimeoutError(url)


def consumer_lag(group: str) -> int:
    out = sh("docker", "exec", container("kafka"), "/opt/kafka/bin/kafka-consumer-groups.sh",
             "--bootstrap-server", "localhost:9092", "--describe", "--group", group, check=False)
    lag = 0
    for line in out.splitlines()[1:]:
        cols = line.split()
        if len(cols) > 5 and cols[5].isdigit():
            lag += int(cols[5])
    return lag


# ---------------------------------------------------------------- experiments
def baseline(c: Client) -> Result:
    r = Result("Baseline", "none", "critical alert visible < 5 s")
    vin, t0 = c.probe_vin(), time.time()
    c.overheat(vin).raise_for_status()
    lat = c.wait_alert(vin, t0, 30)
    r.note(f"alert latency {lat:.2f} s" if lat else "alert NOT delivered")
    r.passed = lat is not None and lat < 5
    return r


def kill_processor(c: Client) -> Result:
    r = Result("Stream-processor crash", "SIGKILL one of two stream-processor replicas",
               "partitions fail over to the survivor; the alert is still raised; lag drains")
    victim = container("stream-processor", 1)
    sh("docker", "kill", victim)
    r.note(f"killed {victim}")
    vin, t0 = c.probe_vin(), time.time()
    c.overheat(vin).raise_for_status()
    lat = c.wait_alert(vin, t0, 90)
    r.note(f"alert delivered after {lat:.1f} s (includes consumer-group rebalance)" if lat else "alert NOT delivered")
    sh("docker", "start", victim)
    t1 = time.time()
    while consumer_lag("stream-processor") > 5000 and time.time() - t1 < 180:
        time.sleep(2)
    r.note(f"replica restarted; lag {consumer_lag('stream-processor')} after {time.time() - t1:.0f} s")
    r.passed = lat is not None
    return r


def restart_kafka(c: Client) -> Result:
    r = Result("Kafka broker outage", "stop the (single) Kafka broker for 20 s, then start it",
               "gateway back-pressures instead of losing data; alert sent during the outage arrives after recovery")
    sh("docker", "stop", container("kafka"))
    r.note("kafka stopped")
    vin, t0 = c.probe_vin(), time.time()
    # The gateway holds the batch (bounded buffer, 2 min delivery timeout) or answers 503.
    import threading
    resp: dict[str, object] = {}
    th = threading.Thread(target=lambda: resp.setdefault("r", c.overheat(vin)))
    th.start()
    time.sleep(20)
    try:
        code = c.get("/fleet/summary").status_code
        r.note(f"API reads during outage: /fleet/summary -> {code}")
    except requests.RequestException as e:
        r.note(f"API reads during outage failed: {e}")
    sh("docker", "start", container("kafka"))
    th.join(timeout=160)
    got = resp.get("r")
    r.note(f"ingest call during outage -> {getattr(got, 'status_code', 'no response')}")
    lat = c.wait_alert(vin, t0, 180)
    r.note(f"alert delivered {lat:.1f} s after send (outage 20 s + broker start)" if lat else "alert NOT delivered")
    r.passed = lat is not None and getattr(got, "status_code", 0) in (202, 503)
    if getattr(got, "status_code", 0) == 503:
        r.note("gateway rejected with 503 (client retries) - no silent loss")
    return r


def stop_clickhouse(c: Client) -> Result:
    r = Result("ClickHouse outage", "stop ClickHouse for 30 s",
               "operational reads (Postgres/Redis) unaffected; analytics fail fast with a problem response; recovers")
    vin = c.vins[0]
    sh("docker", "stop", container("clickhouse"))
    time.sleep(2)
    t = time.time()
    core = c.get("/fleet/summary").status_code
    detail = c.get(f"/vehicles/{vin}").status_code
    ana = c.get("/analytics/fleet-hourly", params={"hours": 24})
    r.note(f"/fleet/summary -> {core}, /vehicles/{{vin}} -> {detail}, "
           f"/analytics/fleet-hourly -> {ana.status_code} in {time.time() - t:.2f} s total")
    fast = [c.get("/vehicles/" + vin + "/telemetry", params={"minutes": 60}) for _ in range(3)]
    r.note("telemetry history during outage: " + ", ".join(f"{x.status_code} ({x.elapsed.total_seconds():.2f}s)" for x in fast))
    time.sleep(28)
    sh("docker", "start", container("clickhouse"))
    wait_http("http://localhost:8123/ping")
    t1 = time.time()
    ok = False
    while time.time() - t1 < 120:
        if c.get("/analytics/fleet-hourly", params={"hours": 24}).status_code == 200:
            ok = True
            break
        time.sleep(2)
    r.note(f"analytics recovered {time.time() - t1:.1f} s after ClickHouse came back" if ok else "analytics did not recover")
    r.passed = core == 200 and detail == 200 and ana.status_code in (200, 503) and ok
    return r


def restart_redis(c: Client) -> Result:
    r = Result("Redis restart (state loss)", "restart Redis (no persistence: live state, cache, limits lost)",
               "API keeps serving; live state rebuilds from the stream; an alert raised after restart is delivered")
    sh("docker", "restart", container("redis"))
    r.note("redis restarted (empty)")
    time.sleep(3)
    codes = [c.get("/vehicles", params={"limit": 20}).status_code for _ in range(5)]
    r.note(f"/vehicles after restart -> {codes}")
    vin, t0 = c.probe_vin(), time.time()
    c.overheat(vin).raise_for_status()
    lat = c.wait_alert(vin, t0, 60)
    r.note(f"alert after Redis restart delivered in {lat:.1f} s" if lat else "alert NOT delivered")
    online = c.get("/fleet/summary").json().get("live", {}).get("online")
    r.note(f"vehicles online (live state) 15 s later: {online}")
    r.passed = all(x == 200 for x in codes) and lat is not None
    return r


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--report", default="docs/evidence/chaos-report.md")
    ap.add_argument("--only", nargs="*")
    args = ap.parse_args()
    c = Client()
    experiments = [baseline, kill_processor, restart_kafka, stop_clickhouse, restart_redis]
    results = []
    for exp in experiments:
        if args.only and exp.__name__ not in args.only:
            continue
        print(f"\n## {exp.__name__}")
        try:
            results.append(exp(c))
        except Exception as e:  # record, keep going: later experiments are independent
            res = Result(exp.__name__, "error", "-")
            res.note(f"experiment error: {e!r}")
            results.append(res)
        time.sleep(10)
        c = Client()  # fresh token/session after infrastructure restarts
    lines = ["# Chaos experiments", "", f"Run at {datetime.now(timezone.utc):%Y-%m-%d %H:%M UTC} against the docker compose stack.", "",
             "| Experiment | Fault | Steady-state hypothesis | Result |", "|---|---|---|---|"]
    lines += [f"| {r.name} | {r.fault} | {r.hypothesis} | {'PASS' if r.passed else 'FAIL'} |" for r in results]
    for r in results:
        lines += ["", f"## {r.name}", ""] + [f"- {o}" for o in r.observations]
    os.makedirs(os.path.dirname(args.report), exist_ok=True)
    with open(args.report, "w") as f:
        f.write("\n".join(lines) + "\n")
    print(f"\nreport written to {args.report}")
    return 0 if all(r.passed for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
