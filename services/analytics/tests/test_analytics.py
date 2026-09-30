from datetime import date, timedelta

import numpy as np
import pandas as pd
import pytest

from analytics import features as F
from analytics import kb
from analytics.embeddings import embed
from analytics.jobs import _Corrected
from analytics.model import (RiskModel, baseline_scores, evaluate, net_savings, precision_recall_at_k)


def synthetic(n_vehicles=600, days=40, seed=0) -> tuple[pd.DataFrame, pd.DataFrame]:
    """Vehicle-days where degrading vehicles show rising coolant / falling
    voltage before a breakdown, plus benign DTC noise everywhere."""
    rng = np.random.default_rng(seed)
    start = date(2026, 8, 1)
    rows, bds = [], []
    for v in range(n_vehicles):
        vin = f"VIN{v:014d}"
        comp = rng.choice(["cooling", "battery12v", None], p=[0.12, 0.12, 0.76])
        fail_day = int(rng.integers(10, days)) if comp else None
        if comp:
            bds.append({"vin": vin, "component": comp, "day": start + timedelta(days=fail_day)})
        for d in range(days):
            sev = 0.0
            if comp and fail_day - 10 <= d < fail_day:
                sev = (d - (fail_day - 10)) / 10
            row = {"vin": vin, "day": start + timedelta(days=d), "odo": 1000 + d * 100, "model_year": 2021, "powertrain": "ICE",
                   "tenant_id": "t"}
            base_cool = 90 + rng.normal(0, 1)
            row.update({
                "max_coolant_7d": base_cool + 5 + (15 * sev if comp == "cooling" else 0),
                "avg_coolant_3d": base_cool + (12 * sev if comp == "cooling" else 0), "avg_coolant_14d": base_cool,
                "min_batt_v_7d": 13.4 - (1.5 * sev if comp == "battery12v" else 0) + rng.normal(0, 0.1),
                "avg_batt_v_3d": 14.0 - (1.2 * sev if comp == "battery12v" else 0), "avg_batt_v_14d": 14.0,
                "max_pack_temp_7d": np.nan, "avg_pack_3d": np.nan, "avg_pack_14d": np.nan,
                "dtc_cooling_7d": rng.poisson(0.05 + (3 * sev if comp == "cooling" else 0)),
                "dtc_battery_7d": rng.poisson(0.05 + (3 * sev if comp == "battery12v" else 0)),
                "dtc_misfire_7d": 0, "dtc_evpack_7d": 0, "dtc_total_3d": rng.poisson(0.3), "dtc_cooling_3d": 0,
                "dtc_battery_3d": 0, "dtc_misfire_3d": 0, "dtc_evpack_3d": 0,
                "harsh_7d": rng.poisson(3), "km_7d": 900 + rng.normal(0, 50), "idle_ratio_7d": 0.1, "days_active_7d": 6,
            })
            row["dtc_total_7d"] = row["dtc_cooling_7d"] + row["dtc_battery_7d"] + rng.poisson(0.4)
            rows.append(row)
    raw = pd.DataFrame(rows)
    return raw, pd.DataFrame(bds, columns=["vin", "component", "day"])


@pytest.fixture(scope="module")
def data():
    raw, bd = synthetic()
    meta = raw[["vin", "tenant_id", "model_year", "powertrain"]].drop_duplicates()
    df = F.add_labels(F.assemble(raw.drop(columns=["tenant_id", "model_year", "powertrain"]), meta, date(2026, 9, 30)), bd)
    split = date(2026, 8, 25)
    return df[df["day"] < split], df[df["day"] >= split]


def test_labels_look_forward_only():
    raw = pd.DataFrame({"vin": ["A"] * 10, "day": [date(2026, 1, 1) + timedelta(days=i) for i in range(10)]})
    bd = pd.DataFrame({"vin": ["A"], "component": ["cooling"], "day": [date(2026, 1, 8)]})
    lab = F.add_labels(raw, bd, horizon=7)
    # day index 7 is the breakdown day itself: label=0 there and after; days 0..6 are within (d, d+7].
    assert lab["label_cooling"].tolist() == [1, 1, 1, 1, 1, 1, 1, 0, 0, 0]
    assert lab["label_any"].sum() == 7 and lab["label_battery12v"].sum() == 0


def test_labels_empty_breakdowns():
    raw = pd.DataFrame({"vin": ["A"], "day": [date(2026, 1, 1)]})
    assert F.add_labels(raw, pd.DataFrame(columns=["vin", "component", "day"]))["label_any"].sum() == 0


def test_assemble_derives_trends_and_types(data):
    train, _ = data
    assert set(F.FEATURES) <= set(train.columns)
    assert train["coolant_trend"].dtype == np.float32
    assert (train["is_ev"] == 0).all() and train["age_years"].iloc[0] == 5


def test_model_beats_rule_baselines_on_later_period(data):
    train, test = data
    m = RiskModel().fit(train)
    rep = evaluate(m, test, capacity_share=0.05)
    model, rules = rep["methods"]["model"], rep["methods"]["threshold_alert_rules"]
    assert model["pr_auc"] > rules["pr_auc"]
    assert model["precision_at_k"] >= rules["precision_at_k"]
    assert rep["positives"] > 0 and len(rep["calibration"]) == 10
    assert set(rep["components"]) <= set(F.COMPONENTS)


def test_prior_correction_lowers_probabilities(data):
    train, test = data
    m = RiskModel().fit(train)
    m.prior_correction = 0.25
    raw = m.predict(test)["risk_7d"]
    corrected = _Corrected(m).predict(test)["risk_7d"]
    assert (corrected <= raw + 1e-9).all()
    assert corrected.between(0, 1).all()


def test_ev_cannot_fail_on_cooling(data):
    train, test = data
    m = RiskModel().fit(train)
    ev = test.head(20).copy()
    ev["is_ev"] = 1
    comps = m.predict_components(ev)
    assert (comps["cooling"] == 0).all() and (comps["misfire"] == 0).all()


def test_explanations_are_readable(data):
    train, _ = data
    m = RiskModel().fit(train)
    row = pd.Series({"coolant_trend": 9.0, "max_coolant_7d": 112.0, "avg_coolant_3d": 101.0, "dtc_cooling_7d": 4.0,
                     "dtc_cooling_3d": 2.0})
    reasons = m.explain(row, "cooling")
    assert reasons and "Coolant running +9.0 °C" in reasons[0]["label"]
    assert m.explain(pd.Series({"coolant_trend": np.nan}), "cooling") == []


def test_precision_recall_at_k_and_savings():
    y = np.array([1, 0, 1, 0, 0])
    s = np.array([0.9, 0.8, 0.7, 0.1, 0.0])
    assert precision_recall_at_k(y, s, 2) == (0.5, 0.5)
    df = pd.DataFrame({f"label_{c}": [0] * 3 for c in F.COMPONENTS})
    df.loc[0, "label_cooling"] = 1
    assert net_savings(df, np.array([True, True, False])) == (2400 - 520) - 80


def test_baselines_rank_dtc_counts():
    df = pd.DataFrame({"dtc_total_7d": [0, 3, 1], "max_coolant_7d": [90, 106, 90], "min_batt_v_7d": [13, 13, 11.5],
                       "max_pack_temp_7d": [np.nan] * 3, "dtc_cooling_7d": [0, 0, 0], "dtc_evpack_7d": [0, 0, 0]})
    b = baseline_scores(df)
    assert b["any_dtc_last_7d"].argmax() == 1
    assert list(b["threshold_alert_rules"] >= 1) == [False, True, True]


def test_knowledge_base_documents():
    docs = kb.documents()
    assert len(docs) >= 25
    assert len({d["kb_id"] for d in docs}) == len(docs)
    p0217 = next(d for d in docs if d["dtc_code"] == "P0217")
    q = embed("engine overheating coolant P0217")
    near = embed(p0217["title"] + " " + p0217["content"])
    far = embed(next(d for d in docs if d["dtc_code"] == "C0750")["content"])
    assert sum(a * b for a, b in zip(q, near)) > sum(a * b for a, b in zip(q, far))
