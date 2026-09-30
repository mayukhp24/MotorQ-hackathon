"""CLI and lightweight scheduler for the analytics jobs.

    python -m analytics.run kb|train|score|refresh|all
    python -m analytics.run schedule     # long-running: all jobs on their cadence

In Kubernetes each job is a CronJob (deploy/helm); in docker compose the
`schedule` mode runs them in one container.
"""

from __future__ import annotations

import logging
import sys
import time
from datetime import datetime, timezone

from prometheus_client import start_http_server

from analytics import jobs
from analytics.config import Config

log = logging.getLogger("analytics")


def run_all(cfg: Config) -> None:
    jobs.build_kb(cfg)
    if jobs.load_active(cfg) is None:
        jobs.train(cfg)
    jobs.score(cfg)
    jobs.refresh_read_models(cfg)


def schedule(cfg: Config) -> None:
    start_http_server(cfg.metrics_port)
    for attempt in range(60):  # wait for the seed to land
        try:
            run_all(cfg)
            break
        except Exception as e:  # noqa: BLE001 - log and retry the whole bootstrap
            log.warning("bootstrap attempt %d failed: %s", attempt, e)
            time.sleep(30)
    last_hour, last_day, last_week = -1, -1, -1
    while True:
        now = datetime.now(timezone.utc)
        try:
            if now.hour != last_hour:
                jobs.refresh_read_models(cfg)
                last_hour = now.hour
            if now.timetuple().tm_yday != last_day and now.hour >= 0 and now.minute >= 30:
                jobs.score(cfg, backfill_days=1)
                last_day = now.timetuple().tm_yday
            if now.isocalendar().week != last_week and now.weekday() == 6:
                jobs.train(cfg)
                last_week = now.isocalendar().week
        except Exception:
            log.exception("scheduled job failed")
        time.sleep(60)


def main(argv: list[str]) -> int:
    logging.basicConfig(level=logging.INFO, format='{"ts":"%(asctime)s","level":"%(levelname)s","msg":"%(message)s"}')
    cfg = Config()
    cmd = argv[1] if len(argv) > 1 else "all"
    if cmd == "kb":
        print(jobs.build_kb(cfg))
    elif cmd == "train":
        res = jobs.train(cfg)
        print({k: res[k] for k in ("version", "methods", "lift_vs_rules")})
    elif cmd == "score":
        print(jobs.score(cfg, int(argv[2]) if len(argv) > 2 else None))
    elif cmd == "refresh":
        jobs.refresh_read_models(cfg)
    elif cmd == "all":
        run_all(cfg)
    elif cmd == "schedule":
        schedule(cfg)
    else:
        print(__doc__)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
