"""Feature engineering for predictive maintenance.

Point-in-time correct: the features for (vehicle, day d) use only daily
aggregates from days <= d (rolling windows over the ClickHouse feature store),
and the label looks strictly forward (breakdown in (d, d+7]). Heavy lifting –
merging aggregate states and 3/7/14-day windows over millions of vehicle-days –
runs inside ClickHouse; pandas only receives the final feature matrix.
"""

from __future__ import annotations

import io
from datetime import date, timedelta

import httpx
import numpy as np
import pandas as pd
import psycopg

from analytics.config import Config

FEATURE_SQL = """
SELECT * FROM (
  SELECT vin, day, odo,
    max(mc)  OVER w7  AS max_coolant_7d,  avg(ac)  OVER w3  AS avg_coolant_3d, avg(ac) OVER w14 AS avg_coolant_14d,
    min(mbv) OVER w7  AS min_batt_v_7d,   avg(abv) OVER w3  AS avg_batt_v_3d,  avg(abv) OVER w14 AS avg_batt_v_14d,
    max(mpt) OVER w7  AS max_pack_temp_7d, avg(mpt) OVER w3 AS avg_pack_3d,    avg(mpt) OVER w14 AS avg_pack_14d,
    sum(dc)  OVER w7  AS dtc_cooling_7d,  sum(db)  OVER w7  AS dtc_battery_7d, sum(dm)  OVER w7  AS dtc_misfire_7d,
    sum(de)  OVER w7  AS dtc_evpack_7d,   sum(dt)  OVER w7  AS dtc_total_7d,   sum(dt)  OVER w3  AS dtc_total_3d,
    sum(dc)  OVER w3  AS dtc_cooling_3d,  sum(db)  OVER w3  AS dtc_battery_3d, sum(dm)  OVER w3  AS dtc_misfire_3d,
    sum(de)  OVER w3  AS dtc_evpack_3d,
    sum(harsh) OVER w7 AS harsh_7d, sum(km) OVER w7 AS km_7d,
    sum(idle) OVER w7 / nullIf(sum(ign) OVER w7, 0) AS idle_ratio_7d,
    count() OVER w7 AS days_active_7d
  FROM (
    SELECT vin, day, toUInt32(day) AS dn,
      max(odo_max) AS odo, max(odo_max) - min(odo_min) AS km,
      sum(ign_samples) AS ign, sum(idle_samples) AS idle,
      max(max_coolant) AS mc, sum(sum_coolant) / nullIf(sum(n_coolant), 0) AS ac,
      min(min_batt_v) AS mbv, sum(sum_batt_v) / nullIf(sum(n_batt_v), 0) AS abv,
      max(max_pack_temp) AS mpt, sum(harsh_events) AS harsh, sum(dtc_total) AS dt,
      sum(dtc_cooling) AS dc, sum(dtc_battery) AS db, sum(dtc_misfire) AS dm, sum(dtc_evpack) AS de
    FROM fleet.vehicle_daily
    WHERE day BETWEEN {lookback:Date} AND {to:Date}
    GROUP BY vin, day
  )
  WINDOW w3  AS (PARTITION BY vin ORDER BY dn RANGE BETWEEN 2 PRECEDING AND CURRENT ROW),
         w7  AS (PARTITION BY vin ORDER BY dn RANGE BETWEEN 6 PRECEDING AND CURRENT ROW),
         w14 AS (PARTITION BY vin ORDER BY dn RANGE BETWEEN 13 PRECEDING AND CURRENT ROW)
)
WHERE day BETWEEN {from:Date} AND {to:Date} AND ({every:UInt8} = 1 OR toUInt32(day) % {every:UInt8} = 0)
SETTINGS max_memory_usage = 8000000000
FORMAT Parquet"""

FEATURES = [
    "age_years", "odo", "is_ev", "is_hev",
    "max_coolant_7d", "avg_coolant_3d", "coolant_trend",
    "min_batt_v_7d", "avg_batt_v_3d", "batt_v_trend",
    "max_pack_temp_7d", "avg_pack_3d", "pack_trend",
    "dtc_cooling_7d", "dtc_battery_7d", "dtc_misfire_7d", "dtc_evpack_7d", "dtc_total_7d", "dtc_total_3d",
    "dtc_cooling_3d", "dtc_battery_3d", "dtc_misfire_3d", "dtc_evpack_3d",
    "harsh_7d", "km_7d", "idle_ratio_7d", "days_active_7d",
]

COMPONENTS = ["cooling", "battery12v", "misfire", "ev_pack"]

# Unplanned breakdown vs planned repair cost (USD); mirrors the simulator's
# cost model and industry ranges (tow + repair + downtime).
BREAKDOWN_COST = {"cooling": 2400, "battery12v": 650, "misfire": 1900, "ev_pack": 7800}
PLANNED_COST = {"cooling": 520, "battery12v": 180, "misfire": 450, "ev_pack": 2600}


def clickhouse_features(cfg: Config, start: date, end: date, every: int = 1) -> pd.DataFrame:
    """Rolling features for every vehicle-day in [start, end] (optionally every Nth day)."""
    params = {"param_lookback": (start - timedelta(days=14)).isoformat(), "param_from": start.isoformat(),
              "param_to": end.isoformat(), "param_every": every}
    with httpx.Client(timeout=600, auth=(cfg.clickhouse_user, cfg.clickhouse_password)) as c:
        r = c.post(cfg.clickhouse_url, params=params, content=FEATURE_SQL.encode())
        if r.status_code != 200:
            raise RuntimeError(f"clickhouse {r.status_code}: {r.text[:500]}")
    df = pd.read_parquet(io.BytesIO(r.content))
    # Parquet carries FixedString as bytes and Date as days since the epoch.
    df["vin"] = df["vin"].map(lambda v: v.decode() if isinstance(v, bytes) else str(v)).str.strip("\x00")
    if pd.api.types.is_integer_dtype(df["day"]):
        df["day"] = pd.to_datetime(df["day"].astype("int64"), unit="D").dt.date
    else:
        df["day"] = pd.to_datetime(df["day"]).dt.date
    return df


def vehicle_meta(conn: psycopg.Connection) -> pd.DataFrame:
    rows = conn.execute(
        """SELECT v.vin, v.tenant_id::text, v.model_year, m.powertrain
           FROM vehicle v JOIN vehicle_model m ON m.model_id = v.model_id""").fetchall()
    return pd.DataFrame(rows, columns=["vin", "tenant_id", "model_year", "powertrain"])


def breakdowns(conn: psycopg.Connection) -> pd.DataFrame:
    rows = conn.execute(
        """SELECT vin, component, occurred_at::date FROM service_event WHERE event_type = 'BREAKDOWN'""").fetchall()
    return pd.DataFrame(rows, columns=["vin", "component", "day"])


def assemble(raw: pd.DataFrame, meta: pd.DataFrame, today: date) -> pd.DataFrame:
    df = raw.merge(meta, on="vin", how="inner")
    df["age_years"] = today.year - df["model_year"]
    df["is_ev"] = (df["powertrain"] == "EV").astype(np.int8)
    df["is_hev"] = (df["powertrain"] == "HEV").astype(np.int8)
    df["coolant_trend"] = df["avg_coolant_3d"] - df["avg_coolant_14d"]
    df["batt_v_trend"] = df["avg_batt_v_3d"] - df["avg_batt_v_14d"]
    df["pack_trend"] = df["avg_pack_3d"] - df["avg_pack_14d"]
    for c in FEATURES:
        df[c] = pd.to_numeric(df[c], errors="coerce").astype("float32")
    return df


def add_labels(df: pd.DataFrame, bd: pd.DataFrame, horizon: int = 7) -> pd.DataFrame:
    """label_<component> = 1 if that component breaks down in (d, d+horizon]."""
    out = df.copy()
    for comp in COMPONENTS:
        out[f"label_{comp}"] = 0
    if bd.empty:
        out["label_any"] = 0
        return out
    keyed = out[["vin", "day"]].reset_index()
    for comp in COMPONENTS:
        ev = bd[bd["component"] == comp][["vin", "day"]].rename(columns={"day": "bday"})
        m = keyed.merge(ev, on="vin")
        delta = (pd.to_datetime(m["bday"]) - pd.to_datetime(m["day"])).dt.days
        hit = m.loc[(delta > 0) & (delta <= horizon), "index"].unique()
        out.loc[hit, f"label_{comp}"] = 1
    out["label_any"] = out[[f"label_{c}" for c in COMPONENTS]].max(axis=1)
    return out
