import os

import requests


def before_all(context):
    context.base = os.environ.get("BASE_URL", "http://localhost:3000")
    context.ingest = os.environ.get("INGEST_URL", "http://localhost:8080")
    context.password = os.environ.get("DEMO_PASSWORD", "FleetPulse!2026")
    keys = os.environ.get("INGEST_API_KEYS", "aurora:aurora-local-key,pinnacle:pinnacle-local-key,stellar:stellar-local-key")
    context.keys = dict(kv.split(":", 1) for kv in keys.split(","))
    context.http = requests.Session()
