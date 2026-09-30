"""Privacy controls applied at the API boundary (data minimisation).

- Location: callers without `location:precise` receive the centre of the
  enclosing geohash-5 cell (~4.9 km x 4.9 km) instead of raw GPS.
- Drivers: callers without `driver:pii` see a stable pseudonym.
"""

from __future__ import annotations

import hashlib
import hmac

_B32 = "0123456789bcdefghjkmnpqrstuvwxyz"


def geohash(lat: float, lon: float, precision: int = 5) -> str:
    lat_lo, lat_hi, lon_lo, lon_hi = -90.0, 90.0, -180.0, 180.0
    out, ch, bit, even = [], 0, 0, True
    while len(out) < precision:
        if even:
            mid = (lon_lo + lon_hi) / 2
            if lon >= mid:
                ch |= 1 << (4 - bit)
                lon_lo = mid
            else:
                lon_hi = mid
        else:
            mid = (lat_lo + lat_hi) / 2
            if lat >= mid:
                ch |= 1 << (4 - bit)
                lat_lo = mid
            else:
                lat_hi = mid
        even = not even
        if bit < 4:
            bit += 1
        else:
            out.append(_B32[ch])
            bit, ch = 0, 0
    return "".join(out)


def geohash_center(gh: str) -> tuple[float, float]:
    lat_lo, lat_hi, lon_lo, lon_hi = -90.0, 90.0, -180.0, 180.0
    even = True
    for c in gh:
        idx = _B32.index(c)
        for b in range(4, -1, -1):
            on = bool(idx & (1 << b))
            if even:
                mid = (lon_lo + lon_hi) / 2
                lon_lo, lon_hi = (mid, lon_hi) if on else (lon_lo, mid)
            else:
                mid = (lat_lo + lat_hi) / 2
                lat_lo, lat_hi = (mid, lat_hi) if on else (lat_lo, mid)
            even = not even
    return (lat_lo + lat_hi) / 2, (lon_lo + lon_hi) / 2


def mask_location(lat: float | None, lon: float | None, precise: bool) -> tuple[float | None, float | None]:
    if lat is None or lon is None or precise:
        return lat, lon
    return geohash_center(geohash(lat, lon, 5))


def pseudonym(driver_id: str | None, secret: str) -> str | None:
    if not driver_id:
        return None
    digest = hmac.new(secret.encode(), driver_id.encode(), hashlib.sha256).hexdigest()[:6]
    return f"Driver #{digest.upper()}"
