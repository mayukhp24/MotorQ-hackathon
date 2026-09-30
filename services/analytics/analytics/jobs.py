"""Batch jobs: train, evaluate, score, refresh read models."""

from __future__ import annotations

import json
import logging
import os
import pickle
import time
from datetime import date, datetime, timedelta, timezone

import numpy as np
import pandas as pd
import psycopg
from prometheus_client import Gauge

from analytics import features as F
from analytics import kb
from analytics.config import Config
from analytics.model import RiskModel, evaluate

log = logging.getLogger("analytics")
JOB_DURATION = Gauge("analytics_job_duration_seconds", "Last job duration", ["job"])
JOB_LAST_SUCCESS = Gauge("analytics_job_last_success_timestamp", "Last successful run", ["job"])
MODEL_METRIC = Gauge("analytics_model_metric", "Latest evaluation metrics", ["method", "metric"])


def today_utc() -> date:
    return datetime.now(timezone.utc).date()


def connect(cfg: Config) -> psycopg.Connection:
    return psycopg.connect(cfg.database_url, autocommit=False)


def _timed(job: str):
    def deco(fn):
        def wrap(*a, **kw):
            t0 = time.time()
            res = fn(*a, **kw)
            JOB_DURATION.labels(job).set(time.time() - t0)
            JOB_LAST_SUCCESS.labels(job).set(time.time())
            log.info("job %s finished in %.1fs", job, time.time() - t0)
            return res
        return wrap
    return deco


@_timed("train")
def train(cfg: Config) -> dict:
    """Time-split training: fit on the earlier period, evaluate on the later
    one (no shuffling across time: that would leak the future)."""
    t = today_utc()
    last_labelled = t - timedelta(days=8)          # labels need 7 future days
    with connect(cfg) as conn:
        first = conn.execute("SELECT min(occurred_at)::date FROM service_event").fetchone()[0]
        meta = F.vehicle_meta(conn)
        bd = F.breakdowns(conn)
    if first is None:
        raise RuntimeError("no maintenance history: run the seed first")
    start = max(first, t - timedelta(days=60)) + timedelta(days=14)  # need 14 days of lookback
    split = last_labelled - timedelta(days=14)
    log.info("building features %s..%s (split at %s)", start, last_labelled, split)
    raw = F.clickhouse_features(cfg, start, last_labelled, every=2)
    df = F.add_labels(F.assemble(raw, meta, t), bd)
    train_df, test_df = df[df["day"] < split], df[df["day"] >= split]
    # Down-sample negatives in training only (rare-event problem); the test
    # set keeps the true base rate so metrics are honest.
    rng = np.random.default_rng(42)
    keep = (train_df["label_any"] == 1) | (rng.random(len(train_df)) < cfg.negative_sample_rate)
    train_s = train_df[keep]
    log.info("train rows %d (pos %d), test rows %d (pos %d)", len(train_s), int(train_s["label_any"].sum()),
             len(test_df), int(test_df["label_any"].sum()))
    model = RiskModel().fit(train_s)
    # Probabilities were learned on down-sampled negatives: correct the prior
    # back to the true base rate so risk_7d is a calibrated probability.
    model.prior_correction = cfg.negative_sample_rate
    report = evaluate(_Corrected(model), test_df)
    report.update({"train_rows": int(len(train_s)), "train_positives": int(train_s["label_any"].sum()),
                   "train_period": [str(start), str(split - timedelta(days=1))],
                   "test_period": [str(split), str(last_labelled)], "features": model.features,
                   "negative_sample_rate": cfg.negative_sample_rate})
    version = "pm-" + datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    os.makedirs(cfg.artifacts_dir, exist_ok=True)
    with open(os.path.join(cfg.artifacts_dir, f"{version}.pkl"), "wb") as fh:
        pickle.dump(model, fh)
    with open(os.path.join(cfg.artifacts_dir, f"{version}.metrics.json"), "w") as fh:
        json.dump(report, fh, indent=2, default=str)
    with connect(cfg) as conn:
        conn.execute("UPDATE model_version SET is_active = false WHERE is_active")
        conn.execute("""INSERT INTO model_version (model_version, algorithm, metrics, features, is_active)
                        VALUES (%s, %s, %s, %s, true)""",
                     (version, "HistGradientBoostingClassifier x4 (per component), noisy-OR combination",
                      json.dumps(report, default=str), json.dumps(model.features)))
        conn.commit()
    for name, m in report["methods"].items():
        for metric in ("pr_auc", "roc_auc", "precision_at_k", "recall_at_k"):
            MODEL_METRIC.labels(name, metric).set(m[metric])
    log.info("model %s: %s", version, json.dumps(report["methods"]))
    return {"version": version, **report}


class _Corrected:
    """Wraps a RiskModel so component probabilities are prior-corrected:
    p = p_s * r / (p_s * r + 1 - p_s), where r is the negative sample rate."""

    def __init__(self, model: RiskModel) -> None:
        self.m = model

    def predict_components(self, df: pd.DataFrame) -> pd.DataFrame:
        comp = self.m.predict_components(df)
        r = getattr(self.m, "prior_correction", 1.0)
        return (comp * r) / (comp * r + 1.0 - comp)

    def predict(self, df: pd.DataFrame) -> pd.DataFrame:
        comp = self.predict_components(df)
        res = pd.DataFrame(index=df.index)
        res["risk_7d"] = 1.0 - np.prod(1.0 - comp[F.COMPONENTS].to_numpy(), axis=1)
        res["top_component"] = comp[F.COMPONENTS].idxmax(axis=1)
        res["top_component_p"] = comp[F.COMPONENTS].max(axis=1)
        return res

    def explain(self, row: pd.Series, component: str, k: int = 3) -> list[dict]:
        return self.m.explain(row, component, k)


def load_active(cfg: Config) -> tuple[str, _Corrected] | None:
    with connect(cfg) as conn:
        row = conn.execute("SELECT model_version FROM model_version WHERE is_active").fetchone()
    if not row:
        return None
    path = os.path.join(cfg.artifacts_dir, f"{row[0]}.pkl")
    if not os.path.exists(path):
        return None
    with open(path, "rb") as fh:
        return row[0], _Corrected(pickle.load(fh))


@_timed("score")
def score(cfg: Config, backfill_days: int | None = None) -> int:
    """Score every vehicle for the most recent complete day(s) and publish
    risk, top failure mode, reasons and $ impact to PostgreSQL."""
    loaded = load_active(cfg)
    if loaded is None:
        raise RuntimeError("no active model artifact: run train first")
    version, model = loaded
    t = today_utc()
    days = backfill_days if backfill_days is not None else cfg.score_backfill_days
    end = t - timedelta(days=1)
    start = end - timedelta(days=max(0, days - 1))
    with connect(cfg) as conn:
        meta = F.vehicle_meta(conn)
    raw = F.clickhouse_features(cfg, start, end, every=1)
    df = F.assemble(raw, meta, t)
    pred = model.predict(df)
    df = df.join(pred)
    rows = []
    for r in df.itertuples(index=False):
        comp = r.top_component
        risk = float(r.risk_7d)
        factors = model.explain(pd.Series(r._asdict()), comp) if risk >= 0.05 else []
        cost = F.BREAKDOWN_COST[comp]
        savings = risk * (cost - F.PLANNED_COST[comp])
        # Scores are published for the day after the features (the prediction date).
        rows.append((r.vin, r.day + timedelta(days=1), version, r.tenant_id, round(risk, 5), comp,
                     json.dumps(factors), cost, round(savings, 2)))
    with connect(cfg) as conn:
        with conn.cursor() as cur:
            cur.execute("CREATE TEMP TABLE rs_stage (LIKE risk_score INCLUDING DEFAULTS) ON COMMIT DROP")
            with cur.copy("""COPY rs_stage (vin, scored_on, model_version, tenant_id, risk_7d, top_component,
                             top_factors, est_breakdown_cost, expected_savings_usd) FROM STDIN""") as cp:
                for row in rows:
                    cp.write_row(row)
            cur.execute("""INSERT INTO risk_score SELECT * FROM rs_stage
                           ON CONFLICT (vin, scored_on, model_version) DO UPDATE
                           SET risk_7d = EXCLUDED.risk_7d, top_component = EXCLUDED.top_component,
                               top_factors = EXCLUDED.top_factors, expected_savings_usd = EXCLUDED.expected_savings_usd""")
        conn.commit()
        conn.execute("SELECT refresh_vehicle_risk()")
        conn.commit()
    log.info("scored %d vehicle-days with %s", len(rows), version)
    return len(rows)


@_timed("read_models")
def refresh_read_models(cfg: Config) -> None:
    with connect(cfg) as conn:
        conn.execute("SELECT refresh_read_models()")
        conn.execute("SELECT ensure_trip_partitions(date_trunc('month', now())::date, 3)")
        conn.commit()


@_timed("knowledge_base")
def build_kb(cfg: Config) -> int:
    with connect(cfg) as conn:
        return kb.build(conn)
