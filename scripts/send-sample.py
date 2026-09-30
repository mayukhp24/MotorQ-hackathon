#!/usr/bin/env python3
"""Post the OEM sample payloads in contracts/oem-samples to the ingest gateway,
re-stamped to the current time (the gateway rejects events older than its
retention window or too far in the future).

    scripts/send-sample.py                    # all three OEMs
    scripts/send-sample.py pinnacle --corrupt # break one VIN check digit -> 1 rejected, sent to the DLQ
"""

from __future__ import annotations

import argparse
import json
import os
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

SAMPLES = Path(__file__).resolve().parents[1] / "contracts" / "oem-samples"
KEYS = dict(kv.split(":", 1) for kv in os.environ.get(
    "INGEST_API_KEYS", "aurora:aurora-local-key,pinnacle:pinnacle-local-key,stellar:stellar-local-key").split(","))


def restamp(oem: str, doc: dict, now_ms: int, corrupt: bool) -> dict:
    items = {"aurora": doc.get("records"), "pinnacle": doc.get("data"), "stellar": doc.get("messages")}[oem]
    for i, r in enumerate(items):
        seq = now_ms * 10 + i  # fresh sequence numbers: not deduplicated as retries
        if oem == "aurora":
            r["timestamp"], r["sequence"] = now_ms, seq
        elif oem == "pinnacle":
            r["EventTime"] = datetime.fromtimestamp(now_ms / 1000, timezone.utc).isoformat(timespec="milliseconds")
            r["Seq"] = seq
        else:
            r["t"], r["n"] = now_ms / 1000, seq
    if corrupt:
        rec = items[-1]
        holder, key = {"aurora": (rec.get("vehicle", {}), "vin"), "pinnacle": (rec, "VIN"), "stellar": (rec, "vin")}[oem]
        v = holder[key]
        holder[key] = v[:8] + ("1" if v[8] != "1" else "2") + v[9:]
    return doc


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("oems", nargs="*", default=["aurora", "pinnacle", "stellar"])
    ap.add_argument("--corrupt", action="store_true", help="corrupt the last record's VIN check digit")
    ap.add_argument("--url", default=os.environ.get("INGEST_URL", "http://localhost:8080"))
    args = ap.parse_args()
    for oem in args.oems:
        doc = restamp(oem, json.loads((SAMPLES / f"{oem}.json").read_text()), int(time.time() * 1000), args.corrupt)
        req = urllib.request.Request(f"{args.url}/v1/ingest/{oem}", data=json.dumps(doc).encode(),
                                     headers={"X-Api-Key": KEYS[oem], "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                print(f"{oem:9s} {r.status} {r.read().decode()}")
        except urllib.error.HTTPError as e:
            print(f"{oem:9s} {e.code} {e.read().decode()[:300]}")


if __name__ == "__main__":
    main()
