// Command stream-processor runs real-time detection, live state and
// enrichment over the canonical telemetry stream.
package main

import (
	"context"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"fleetpulse/pipeline/internal/detect"
	"fleetpulse/pipeline/internal/migrate"
	"fleetpulse/pipeline/internal/platform"
	"fleetpulse/pipeline/internal/processor"
)

func main() {
	log := platform.Logger("stream-processor")
	ctx, cancel := platform.SignalContext()
	defer cancel()

	pool, err := migrate.Connect(ctx, platform.Env("DATABASE_URL", "postgres://fleetpulse_writer@localhost:5432/fleetpulse"), log)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	meta := &processor.PGMeta{Pool: pool}
	n, err := meta.LoadWithRetry(ctx, 100, log)
	if err != nil {
		log.Error("vehicle registry", "err", err)
		os.Exit(1)
	}
	log.Info("vehicle registry loaded", "vehicles", n)
	go meta.Refresh(ctx, platform.EnvDuration("REGISTRY_REFRESH", time.Minute), log)

	opt, err := redis.ParseURL(platform.Env("REDIS_URL", "redis://localhost:6379/0"))
	if err != nil {
		log.Error("redis url", "err", err)
		os.Exit(1)
	}
	opt.PoolSize = 32
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	th := detect.DefaultThresholds()
	th.OverheatC = platform.EnvFloat("RULE_OVERHEAT_C", th.OverheatC)
	th.IdleMs = int64(platform.EnvDuration("RULE_IDLE", time.Duration(th.IdleMs)*time.Millisecond) / time.Millisecond)
	cfg := processor.DefaultConfig()
	cfg.Instance, _ = os.Hostname()
	cfg.DedupPerPart = platform.EnvInt("DEDUP_PER_PARTITION", cfg.DedupPerPart)

	p := processor.New(cfg, detect.NewEngine(th), meta, nil, &processor.RedisLive{R: rdb}, log)
	ready := func() error {
		c, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return rdb.Ping(c).Err()
	}
	platform.ServeOps(ctx, platform.Env("OPS_ADDR", ":9103"), ready, log, nil)

	// Live-state flush and snapshot loops (graceful degradation: a Redis
	// outage delays the dashboard but never blocks detection or Kafka).
	go func() {
		t := time.NewTicker(platform.EnvDuration("LIVE_FLUSH", 500*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.FlushLive(ctx); err != nil {
					log.Warn("live flush failed", "err", err)
				}
			}
		}
	}()
	go func() {
		t := time.NewTicker(platform.EnvDuration("SNAPSHOT_EVERY", 2*time.Second))
		defer t.Stop()
		var last int64
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.Snapshot(ctx); err != nil {
					log.Warn("snapshot failed", "err", err)
				}
				if i%5 == 0 {
					ev := p.Events()
					log.Info("processor throughput", "events_per_sec", (ev-last)/10, "partitions", p.Owned())
					last = ev
				}
			}
		}
	}()
	if err := processor.Run(ctx, p, platform.Env("CONSUMER_GROUP", "stream-processor"), log); err != nil {
		log.Error("processor stopped", "err", err)
		os.Exit(1)
	}
}
