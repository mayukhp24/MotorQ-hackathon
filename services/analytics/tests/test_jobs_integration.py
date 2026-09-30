"""Batch jobs end to end against a real PostgreSQL (all migrations, pgvector,
RLS, read-model functions) via Testcontainers, connecting as the production
least-privilege writer role. Only the ClickHouse feature query is replaced by
synthetic vehicle-day features with a known degradation signal."""

from __future__ import annotations

import json
import uuid
from datetime import date, timedelta
from pathlib import Path

import pytest

docker = pytest.importorskip("docker")
try:
    docker.from_env().ping()
except Exception:  # pragma: no cover - environment without Docker
    pytest.skip("Docker not available", allow_module_level=True)

import psycopg  # noqa: E402
from testcontainers.postgres import PostgresContainer  # noqa: E402

from analytics import features as F  # noqa: E402
from analytics import jobs, kb, run  # noqa: E402
from analytics.config import Config  # noqa: E402
from tests.test_analytics import synthetic  # noqa: E402

REPO = Path(__file__).resolve().parents[3]
START = date(2026, 8, 1)
DAYS = 70
TODAY = START + timedelta(days=DAYS)
TENANT = str(uuid.uuid4())


def vin_for(synthetic_vin: str) -> str:
    return "1FTFW1E5" + synthetic_vin[-9:]  # valid charset (no I/O/Q), 17 chars


@pytest.fixture(scope="module")
def env(tmp_path_factory):
    # Generate past "today" and cut off: on the scoring day some vehicles are
    # mid-degradation with breakdowns that have not happened yet.
    raw, bd = synthetic(n_vehicles=500, days=DAYS + 10, seed=3)
    raw, bd = raw[raw["day"] < TODAY].copy(), bd[bd["day"] < TODAY].copy()
    raw["vin"] = raw["vin"].map(vin_for)
    bd["vin"] = bd["vin"].map(vin_for)
    with PostgresContainer("pgvector/pgvector:pg16", username="postgres", password="postgres",
                           dbname="fleetpulse", driver=None) as pg:
        admin = pg.get_connection_url()
        with psycopg.connect(admin, autocommit=True) as conn:
            for f in sorted((REPO / "db" / "postgres" / "migrations").glob("*.sql")):
                conn.execute(f.read_text())
            conn.execute("ALTER ROLE fleetpulse_writer WITH PASSWORD 'writer'")
            conn.execute("INSERT INTO plan VALUES ('business','Business',7,3000,'{}')")
            conn.execute("INSERT INTO oem VALUES ('aurora','Aurora','AUR',1)")
            for d in kb.documents():  # DTC catalogue normally loaded by the seed
                if d["dtc_code"]:
                    conn.execute("INSERT INTO dtc_code VALUES (%s,%s,'MAJOR',%s) ON CONFLICT DO NOTHING",
                                 (d["dtc_code"], d["title"], d["component"]))
            ice, ev = str(uuid.uuid4()), str(uuid.uuid4())
            conn.execute("INSERT INTO vehicle_model VALUES (%s,'aurora','Cargo','ICE','van',NULL,80)", (ice,))
            conn.execute("INSERT INTO vehicle_model VALUES (%s,'aurora','Volt','EV','van',75,NULL)", (ev,))
            conn.execute("INSERT INTO tenant (tenant_id, slug, name) VALUES (%s,'test-fleet','Test Fleet')", (TENANT,))
            depot, fleet = str(uuid.uuid4()), str(uuid.uuid4())
            conn.execute("INSERT INTO depot VALUES (%s,%s,'Hub','Chennai',13.08,80.27)", (depot, TENANT))
            conn.execute("INSERT INTO fleet VALUES (%s,%s,%s,'Main')", (fleet, TENANT, depot))
            for i, v in enumerate(sorted(raw["vin"].unique())):
                conn.execute("INSERT INTO vehicle VALUES (%s,%s,%s,%s,2021,'TN01',  'ACTIVE','2021-01-01')",
                             (v, TENANT, fleet, ev if i % 10 == 0 else ice))
            conn.execute("""INSERT INTO service_event (service_event_id, tenant_id, vin, event_type, component, occurred_at)
                            VALUES (%s,%s,%s,'PLANNED_SERVICE','general',%s)""",
                         (str(uuid.uuid4()), TENANT, raw["vin"].iloc[0], START))
            for r in bd.itertuples(index=False):
                conn.execute("""INSERT INTO service_event (service_event_id, tenant_id, vin, event_type, component, occurred_at)
                                VALUES (%s,%s,%s,'BREAKDOWN',%s,%s)""", (str(uuid.uuid4()), TENANT, r.vin, r.component, r.day))
        writer = admin.replace("postgres:postgres@", "fleetpulse_writer:writer@")
        yield {"url": writer, "admin": admin, "raw": raw.drop(columns=["tenant_id", "model_year", "powertrain"]),
               "artifacts": str(tmp_path_factory.mktemp("artifacts"))}


@pytest.fixture
def cfg(env, monkeypatch):
    def fake_features(_cfg, start, end, every=1):
        raw = env["raw"]
        days = sorted(d for d in raw["day"].unique() if start <= d <= end)[::every]
        return raw[raw["day"].isin(days)].copy()

    monkeypatch.setattr(F, "clickhouse_features", fake_features)
    monkeypatch.setattr(jobs, "today_utc", lambda: TODAY)
    monkeypatch.setenv("DATABASE_URL", env["url"])
    monkeypatch.setenv("ARTIFACTS_DIR", env["artifacts"])
    return Config()


def test_knowledge_base_is_embedded_and_idempotent(cfg, env):
    n = jobs.build_kb(cfg)
    assert jobs.build_kb(cfg) == n  # upsert, no duplicates
    with psycopg.connect(env["admin"]) as conn:
        rows, dims = conn.execute("SELECT count(*), max(vector_dims(embedding)) FROM fault_knowledge").fetchone()
    assert rows == n >= 25 and dims > 0


def test_no_model_yet(cfg):
    assert jobs.load_active(cfg) is None
    with pytest.raises(RuntimeError, match="no active model"):
        jobs.score(cfg)


def test_train_activates_a_model_that_beats_rules(cfg, env):
    report = jobs.train(cfg)
    m, rules = report["methods"]["model"], report["methods"]["threshold_alert_rules"]
    assert m["pr_auc"] > rules["pr_auc"]
    assert Path(env["artifacts"], f"{report['version']}.pkl").exists()
    with psycopg.connect(env["admin"]) as conn:
        active = conn.execute("SELECT model_version, metrics FROM model_version WHERE is_active").fetchall()
    assert len(active) == 1 and active[0][0] == report["version"]
    assert active[0][1]["test_period"][1] == str(TODAY - timedelta(days=8))  # labels never reach past today-8


def test_score_publishes_calibrated_risk_with_reasons(cfg, env):
    n = jobs.score(cfg, backfill_days=1)
    assert n == env["raw"]["vin"].nunique()
    with psycopg.connect(env["admin"]) as conn:
        stats = conn.execute("""SELECT count(*), min(risk_7d), max(risk_7d), min(scored_on), max(scored_on)
                                FROM risk_score""").fetchone()
        top = conn.execute("""SELECT risk_7d, top_component, top_factors, expected_savings_usd
                              FROM mv_vehicle_risk_current ORDER BY risk_7d DESC LIMIT 1""").fetchone()
        ev_cooling = conn.execute("""SELECT count(*) FROM mv_vehicle_risk_current r JOIN vehicle v USING (vin)
                                     JOIN vehicle_model m USING (model_id)
                                     WHERE m.powertrain = 'EV' AND r.top_component IN ('cooling', 'misfire')""").fetchone()[0]
    count, lo, hi, first, last = stats
    assert count == n and 0 <= lo <= hi <= 1 and first == last == TODAY
    assert top[0] > 0.2 and top[2] and float(top[3]) > 0, "riskiest vehicle needs reasons and a $ impact"
    assert json.loads(json.dumps(top[2]))[0]["label"]
    assert ev_cooling == 0, "EVs cannot be predicted to fail on engine cooling or misfire"
    assert jobs.score(cfg, backfill_days=1) == n  # re-scoring the same day upserts


def test_refresh_read_models(cfg, env):
    jobs.refresh_read_models(cfg)
    with psycopg.connect(env["admin"]) as conn:
        parts = conn.execute("SELECT count(*) FROM pg_inherits WHERE inhparent = 'trip'::regclass").fetchone()[0]
    assert parts >= 3


def test_cli_commands(cfg, capsys):
    assert run.main(["run", "kb"]) == 0
    assert run.main(["run", "refresh"]) == 0
    assert run.main(["run", "score", "1"]) == 0
    assert run.main(["run", "train"]) == 0
    assert "pm-" in capsys.readouterr().out
    assert run.main(["run", "all"]) == 0
    assert run.main(["run", "nope"]) == 2
