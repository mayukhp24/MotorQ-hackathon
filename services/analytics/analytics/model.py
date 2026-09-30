"""Predictive-maintenance model: one gradient-boosted classifier per
component, combined into a 7-day breakdown probability.

Why per component: the maintenance decision needs *what* will fail, and each
failure mode has different precursors (coolant trend vs. voltage sag vs.
misfire codes). The fleet-level risk is P(any) = 1 - Π(1 - p_c).

Evaluated on a strict time split (train on earlier days, test on later days)
against two rule baselines a fleet would use today.
"""

from __future__ import annotations

import numpy as np
import pandas as pd
from sklearn.ensemble import HistGradientBoostingClassifier
from sklearn.metrics import average_precision_score, roc_auc_score

from analytics.features import BREAKDOWN_COST, COMPONENTS, FEATURES, PLANNED_COST

INSPECTION_COST = 80.0  # cost of a false alarm (workshop inspection)

# Features that plausibly relate to each failure mode; used for explanations.
COMPONENT_FEATURES = {
    "cooling": ["coolant_trend", "max_coolant_7d", "avg_coolant_3d", "dtc_cooling_7d", "dtc_cooling_3d"],
    "battery12v": ["batt_v_trend", "min_batt_v_7d", "avg_batt_v_3d", "dtc_battery_7d", "dtc_battery_3d"],
    "misfire": ["dtc_misfire_7d", "dtc_misfire_3d", "dtc_total_3d", "km_7d"],
    "ev_pack": ["pack_trend", "max_pack_temp_7d", "avg_pack_3d", "dtc_evpack_7d", "dtc_evpack_3d"],
}
# Direction in which a deviation indicates degradation.
HIGHER_IS_WORSE = {"batt_v_trend": False, "min_batt_v_7d": False, "avg_batt_v_3d": False}

LABELS = {
    "coolant_trend": "Coolant running {v:+.1f} °C above its 14-day baseline",
    "max_coolant_7d": "Peak coolant {v:.0f} °C in the last 7 days",
    "avg_coolant_3d": "Average coolant {v:.1f} °C over 3 days",
    "dtc_cooling_7d": "{v:.0f} cooling-system fault codes in 7 days",
    "dtc_cooling_3d": "{v:.0f} cooling-system fault codes in 3 days",
    "batt_v_trend": "Charging voltage {v:+.2f} V vs 14-day baseline",
    "min_batt_v_7d": "Minimum system voltage {v:.2f} V",
    "avg_batt_v_3d": "Average charging voltage {v:.2f} V over 3 days",
    "dtc_battery_7d": "{v:.0f} electrical fault codes in 7 days",
    "dtc_battery_3d": "{v:.0f} electrical fault codes in 3 days",
    "dtc_misfire_7d": "{v:.0f} misfire codes in 7 days",
    "dtc_misfire_3d": "{v:.0f} misfire codes in 3 days",
    "dtc_total_3d": "{v:.0f} fault codes in 3 days",
    "km_7d": "{v:.0f} km driven in 7 days",
    "pack_trend": "HV pack {v:+.1f} °C above its 14-day baseline",
    "max_pack_temp_7d": "Peak HV pack temperature {v:.0f} °C",
    "avg_pack_3d": "Average HV pack temperature {v:.1f} °C",
    "dtc_evpack_7d": "{v:.0f} HV battery fault codes in 7 days",
    "dtc_evpack_3d": "{v:.0f} HV battery fault codes in 3 days",
}


def new_classifier(seed: int = 7) -> HistGradientBoostingClassifier:
    return HistGradientBoostingClassifier(max_iter=250, learning_rate=0.08, max_leaf_nodes=31, min_samples_leaf=40,
                                          l2_regularization=1.0, early_stopping=True, validation_fraction=0.15,
                                          n_iter_no_change=20, random_state=seed)


class RiskModel:
    def __init__(self) -> None:
        self.models: dict[str, HistGradientBoostingClassifier] = {}
        self.baseline_mean: dict[str, float] = {}
        self.baseline_std: dict[str, float] = {}
        self.features = list(FEATURES)

    def fit(self, train: pd.DataFrame) -> "RiskModel":
        X = train[self.features].to_numpy(dtype=np.float32)
        for comp in COMPONENTS:
            y = train[f"label_{comp}"].to_numpy()
            if y.sum() < 10:
                continue
            self.models[comp] = new_classifier().fit(X, y)
        healthy = train[train["label_any"] == 0]
        for f in self.features:
            col = healthy[f].astype("float64")
            self.baseline_mean[f] = float(col.mean(skipna=True)) if col.notna().any() else 0.0
            self.baseline_std[f] = float(col.std(skipna=True) or 1.0) if col.notna().any() else 1.0
        return self

    def predict_components(self, df: pd.DataFrame) -> pd.DataFrame:
        X = df[self.features].to_numpy(dtype=np.float32)
        out = pd.DataFrame(index=df.index)
        for comp in COMPONENTS:
            m = self.models.get(comp)
            out[comp] = m.predict_proba(X)[:, 1] if m is not None else 0.0
        # Components that cannot fail on a powertrain get zero probability.
        if "is_ev" in df:
            out.loc[df["is_ev"] == 1, ["cooling", "misfire"]] = 0.0
            out.loc[(df["is_ev"] == 0) & (df["is_hev"] == 0), "ev_pack"] = 0.0
        return out

    def predict(self, df: pd.DataFrame) -> pd.DataFrame:
        comp = self.predict_components(df)
        res = pd.DataFrame(index=df.index)
        res["risk_7d"] = 1.0 - np.prod(1.0 - comp[COMPONENTS].to_numpy(), axis=1)
        res["top_component"] = comp[COMPONENTS].idxmax(axis=1)
        res["top_component_p"] = comp[COMPONENTS].max(axis=1)
        return res

    def explain(self, row: pd.Series, component: str, k: int = 3) -> list[dict]:
        """Top deviations from the healthy-fleet baseline among features
        relevant to the predicted failure mode (human-readable reasons)."""
        scored = []
        for f in COMPONENT_FEATURES.get(component, []):
            v = row.get(f)
            if v is None or pd.isna(v):
                continue
            z = (float(v) - self.baseline_mean.get(f, 0.0)) / (self.baseline_std.get(f, 1.0) or 1.0)
            if not HIGHER_IS_WORSE.get(f, True):
                z = -z
            if z > 0.5:
                scored.append((z, f, float(v)))
        scored.sort(reverse=True)
        return [{"feature": f, "value": round(v, 3), "z": round(z, 2), "label": LABELS.get(f, f).format(v=v)}
                for z, f, v in scored[:k]]


# ------------------------------------------------------------ evaluation
def baseline_scores(df: pd.DataFrame) -> dict[str, np.ndarray]:
    """Rules a fleet uses today, turned into scores for fair comparison."""
    any_dtc = (df["dtc_total_7d"].fillna(0) > 0).astype(float).to_numpy()
    rules = ((df["max_coolant_7d"].fillna(0) >= 104) | (df["min_batt_v_7d"].fillna(99) < 11.8)
             | (df["max_pack_temp_7d"].fillna(0) >= 55) | (df["dtc_cooling_7d"].fillna(0) + df["dtc_evpack_7d"].fillna(0) >= 2)
             ).astype(float).to_numpy()
    return {"any_dtc_last_7d": any_dtc + df["dtc_total_7d"].fillna(0).to_numpy() * 1e-3,
            "threshold_alert_rules": rules + df["dtc_total_7d"].fillna(0).to_numpy() * 1e-3}


def precision_recall_at_k(y: np.ndarray, score: np.ndarray, k: int) -> tuple[float, float]:
    k = max(1, min(k, len(y)))
    idx = np.argsort(-score, kind="stable")[:k]
    tp = y[idx].sum()
    return float(tp / k), float(tp / max(1, y.sum()))


def net_savings(df: pd.DataFrame, flagged: np.ndarray) -> float:
    """$ saved by acting on flagged vehicle-days: avoided breakdown minus
    planned repair for true positives, minus an inspection per false alarm."""
    total = 0.0
    labels = df[[f"label_{c}" for c in COMPONENTS]].to_numpy()
    for i in np.flatnonzero(flagged):
        hits = [COMPONENTS[j] for j in np.flatnonzero(labels[i])]
        if hits:
            total += sum(BREAKDOWN_COST[c] - PLANNED_COST[c] for c in hits)
        else:
            total -= INSPECTION_COST
    return total


def evaluate(model: RiskModel, test: pd.DataFrame, capacity_share: float = 0.01) -> dict:
    """Metrics on the held-out later period. Operating point: the workshop
    can inspect `capacity_share` of vehicle-days (top-k), the same budget
    given to every method."""
    y = test["label_any"].to_numpy()
    pred = model.predict(test)
    k = int(len(test) * capacity_share)
    methods = {"model": pred["risk_7d"].to_numpy(), **baseline_scores(test)}
    report: dict = {"test_rows": int(len(test)), "positives": int(y.sum()), "base_rate": round(float(y.mean()), 5),
                    "capacity_share": capacity_share, "k": k, "methods": {}}
    for name, s in methods.items():
        p_at_k, r_at_k = precision_recall_at_k(y, s, k)
        flagged = np.zeros(len(y), dtype=bool)
        flagged[np.argsort(-s, kind="stable")[:k]] = True
        report["methods"][name] = {
            "pr_auc": round(float(average_precision_score(y, s)), 4),
            "roc_auc": round(float(roc_auc_score(y, s)), 4),
            "precision_at_k": round(p_at_k, 4), "recall_at_k": round(r_at_k, 4),
            "net_savings_usd_in_test_window": round(net_savings(test, flagged)),
        }
    m, b = report["methods"]["model"], report["methods"]["threshold_alert_rules"]
    report["lift_vs_rules"] = {"precision_at_k": round(m["precision_at_k"] / max(b["precision_at_k"], 1e-9), 2),
                               "pr_auc": round(m["pr_auc"] / max(b["pr_auc"], 1e-9), 2)}
    # Per-component discrimination (where the component can fail).
    comps = model.predict_components(test)
    report["components"] = {}
    for c in COMPONENTS:
        yc = test[f"label_{c}"].to_numpy()
        if yc.sum() >= 5 and len(np.unique(yc)) == 2:
            report["components"][c] = {"positives": int(yc.sum()),
                                       "pr_auc": round(float(average_precision_score(yc, comps[c])), 4),
                                       "roc_auc": round(float(roc_auc_score(yc, comps[c])), 4)}
    # Calibration: mean predicted vs observed rate by risk decile.
    dec = pd.qcut(pred["risk_7d"].rank(method="first"), 10, labels=False)
    report["calibration"] = [{"decile": int(d), "predicted": round(float(pred["risk_7d"][dec == d].mean()), 4),
                              "observed": round(float(y[dec.to_numpy() == d].mean()), 4)} for d in range(10)]
    return report
