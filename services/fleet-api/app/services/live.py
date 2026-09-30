"""Live read model (Redis), written by the stream processor.

Per-partition snapshots are merged here; stale partitions (e.g. after a
rebalance or a crashed processor) are ignored so counts never double."""

from __future__ import annotations

import math
import time
from typing import Any

import orjson
from redis.asyncio import Redis

from app.security.masking import geohash_center, mask_location

LIVE_FIELDS = ["st", "spd", "lat", "lon", "ts", "soc", "fuel", "cool", "bv", "pack", "odo", "hdg", "al", "als", "crit", "pt", "ig"]


def _f(v: Any) -> float | None:
    if v in (None, "", b""):
        return None
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def parse_live(values: list[Any]) -> dict[str, Any] | None:
    d = dict(zip(LIVE_FIELDS, values))
    if d.get("st") is None:
        return None
    now_ms = time.time() * 1000
    ts = _f(d.get("ts")) or 0
    crit_until = _f(d.get("crit")) or 0
    return {
        "status": d["st"], "speed_kmh": _f(d["spd"]), "lat": _f(d["lat"]), "lon": _f(d["lon"]),
        "ts_ms": int(ts), "age_s": round((now_ms - ts) / 1000, 1) if ts else None,
        "online": bool(ts and now_ms - ts < 5 * 60_000),
        "soc_pct": _f(d["soc"]), "fuel_pct": _f(d["fuel"]), "coolant_c": _f(d["cool"]), "batt_v": _f(d["bv"]),
        "pack_temp_c": _f(d["pack"]), "odo_km": _f(d["odo"]), "heading": _f(d["hdg"]),
        "last_alert": d.get("al") or None, "last_alert_severity": d.get("als") or None,
        "critical": crit_until > now_ms, "powertrain": d.get("pt"), "ignition": d.get("ig") == "1",
    }


async def live_states(live: Redis, vins: list[str], precise: bool) -> dict[str, dict[str, Any]]:
    if not vins:
        return {}
    pipe = live.pipeline(transaction=False)
    for v in vins:
        pipe.hmget(f"v:{v}", LIVE_FIELDS)
    out: dict[str, dict[str, Any]] = {}
    for vin, vals in zip(vins, await pipe.execute()):
        st = parse_live(vals)
        if st is not None:
            st["lat"], st["lon"] = mask_location(st["lat"], st["lon"], precise)
            out[vin] = st
    return out


async def merged_kpi(live: Redis, tenant_id: str, stale_s: int) -> dict[str, Any]:
    raw = await live.hgetall(f"kpi:{tenant_id}")
    now_ms = time.time() * 1000
    total = {"online": 0, "by_status": {}, "critical": 0, "eps": 0.0, "moving": 0, "speed_sum": 0.0,
             "low_energy": 0, "partitions": 0}
    for _, v in raw.items():
        k = orjson.loads(v)
        if now_ms - k.get("ts_ms", 0) > stale_s * 1000:
            continue
        total["partitions"] += 1
        for f in ("online", "critical", "moving", "low_energy"):
            total[f] += k.get(f, 0)
        total["eps"] += k.get("eps", 0.0)
        total["speed_sum"] += k.get("speed_sum", 0.0)
        for s, n in (k.get("by_status") or {}).items():
            total["by_status"][s] = total["by_status"].get(s, 0) + n
    total["avg_speed_kmh"] = round(total["speed_sum"] / total["moving"], 1) if total["moving"] else 0.0
    total["eps"] = round(total["eps"], 1)
    del total["speed_sum"]
    return total


async def clusters(live: Redis, tenant_id: str, precision: int, stale_s: int) -> list[dict[str, Any]]:
    raw = await live.hgetall(f"clu:{tenant_id}:{precision}")
    now_ms = time.time() * 1000
    cells: dict[str, int] = {}
    for _, v in raw.items():
        snap = orjson.loads(v)
        if now_ms - snap.get("ts_ms", 0) > stale_s * 1000:
            continue
        for gh, n in snap.get("cells", {}).items():
            cells[gh] = cells.get(gh, 0) + n
    out = []
    for gh, n in cells.items():
        lat, lon = geohash_center(gh)
        out.append({"geohash": gh, "lat": round(lat, 5), "lon": round(lon, 5), "count": n})
    out.sort(key=lambda c: -c["count"])
    return out


async def top_dtc(live: Redis, tenant_id: str, stale_s: int, k: int = 10) -> list[dict[str, Any]]:
    raw = await live.hgetall(f"topdtc:{tenant_id}")
    now_ms = time.time() * 1000
    counts: dict[str, int] = {}
    for _, v in raw.items():
        snap = orjson.loads(v)
        if now_ms - snap.get("ts_ms", 0) > stale_s * 1000:
            continue
        for it in snap.get("items", []):
            counts[it["key"]] = counts.get(it["key"], 0) + it["count"]
    return [{"code": c, "count": n} for c, n in sorted(counts.items(), key=lambda x: -x[1])[:k]]


async def vehicles_in_box(live: Redis, tenant_id: str, bbox: tuple[float, float, float, float], limit: int,
                          precise: bool) -> list[dict[str, Any]]:
    min_lon, min_lat, max_lon, max_lat = bbox
    c_lon, c_lat = (min_lon + max_lon) / 2, (min_lat + max_lat) / 2
    width_km = max(0.1, abs(max_lon - min_lon) * 111.32 * max(0.2, abs(math.cos(math.radians(c_lat)))))
    height_km = max(0.1, abs(max_lat - min_lat) * 110.57)
    members = await live.geosearch(f"geo:{tenant_id}", longitude=c_lon, latitude=c_lat, width=width_km,
                                   height=height_km, unit="km", sort="ASC", count=limit)
    vins = [m.decode() if isinstance(m, bytes) else m for m in members]
    states = await live_states(live, vins, precise)
    return [{"vin": v, **s} for v, s in states.items()]
