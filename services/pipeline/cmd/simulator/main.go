// Command simulator generates the synthetic connected fleet.
//
//	simulator seed    load master data + history into PostgreSQL and ClickHouse
//	simulator stream  stream live 1 Hz telemetry for every vehicle (MQTT or HTTPS)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"fleetpulse/pipeline/internal/migrate"
	"fleetpulse/pipeline/internal/platform"
	"fleetpulse/pipeline/internal/seed"
	"fleetpulse/pipeline/internal/sim"
)

func main() {
	log := platform.Logger("simulator")
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: simulator seed|stream")
		os.Exit(2)
	}
	ctx, cancel := platform.SignalContext()
	defer cancel()
	vehicles := platform.EnvInt("SIM_VEHICLES", 100_000)
	w, err := sim.BuildWorld(uint64(platform.EnvInt("SIM_SEED", 2026)), vehicles)
	if err != nil {
		log.Error("world generation failed", "err", err)
		os.Exit(1)
	}
	log.Info("world generated", "vehicles", len(w.Vehicles), "tenants", len(w.Tenants), "drivers", len(w.Drivers))
	switch os.Args[1] {
	case "seed":
		err = runSeed(ctx, w, log)
	case "stream":
		err = runStream(ctx, w, log)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Error("simulator failed", "err", err)
		os.Exit(1)
	}
}

func runSeed(ctx context.Context, w *sim.World, log *slog.Logger) error {
	o := seed.Options{
		HistoryDays:  platform.EnvInt("SEED_HISTORY_DAYS", 60),
		TripDays:     platform.EnvInt("SEED_TRIP_DAYS", 7),
		AlertDays:    platform.EnvInt("SEED_ALERT_DAYS", 30),
		DemoPassword: platform.Env("SEED_DEMO_PASSWORD", "FleetPulse!2026"),
		PIIKey:       platform.Env("PII_ENCRYPTION_KEY", ""),
		Now:          time.Now().UTC(),
		Force:        platform.Env("SEED_FORCE", "false") == "true",
	}
	if o.PIIKey == "" {
		return fmt.Errorf("PII_ENCRYPTION_KEY must be set")
	}
	pool, err := migrate.Connect(ctx, platform.Env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/fleetpulse"), log)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := seed.Postgres(ctx, pool, w, o, log); err != nil {
		return err
	}
	ch := &migrate.ClickHouse{URL: platform.Env("CLICKHOUSE_URL", "http://localhost:8123"),
		User: platform.Env("CLICKHOUSE_USER", "default"), Password: platform.Env("CLICKHOUSE_PASSWORD", "")}
	return seed.ClickHouse(ctx, ch, w, o, log)
}

func runStream(ctx context.Context, w *sim.World, log *slog.Logger) error {
	cfg := sim.DefaultRunnerConfig()
	cfg.Interval = platform.EnvDuration("SIM_INTERVAL", time.Second)
	cfg.BatchSize = platform.EnvInt("SIM_BATCH", 500)
	cfg.Workers = platform.EnvInt("SIM_WORKERS", 8)
	cfg.ActiveShare = platform.EnvFloat("SIM_ACTIVE_SHARE", 0.6)
	cfg.DupRate = platform.EnvFloat("SIM_DUP_RATE", 0.01)
	cfg.OutOfOrder = platform.EnvFloat("SIM_OOO_RATE", 0.02)
	cfg.BadRate = platform.EnvFloat("SIM_BAD_RATE", 0.0005)
	cfg.BurstEvery = platform.EnvDuration("SIM_BURST_EVERY", 0)
	cfg.BurstFor = platform.EnvDuration("SIM_BURST_FOR", 5*time.Minute)
	cfg.BurstFactor = platform.EnvInt("SIM_BURST_FACTOR", 3)
	cfg.OutageEvery = platform.EnvDuration("SIM_OUTAGE_EVERY", 0)
	cfg.OutageFor = platform.EnvDuration("SIM_OUTAGE_FOR", 30*time.Second)
	cfg.Limit = platform.EnvInt("SIM_LIMIT", 0)
	cfg.RateLimitEPS = platform.EnvInt("SIM_MAX_EPS", 0)

	var pub sim.Publisher
	switch strings.ToLower(platform.Env("SIM_TRANSPORT", "mqtt")) {
	case "http":
		pub = sim.NewHTTPPublisher(platform.Env("INGEST_URL", "http://localhost:8080"), platform.Env("INGEST_API_KEYS", ""))
	default:
		mp, err := sim.NewMQTTPublisher(platform.Env("MQTT_URL", "tcp://localhost:1883"), "sim",
			platform.Env("MQTT_USERNAME", ""), platform.Env("MQTT_PASSWORD", ""), platform.EnvInt("SIM_MQTT_CONNS", 4))
		if err != nil {
			return err
		}
		defer mp.Close()
		pub = mp
	}
	r := sim.NewRunner(w, cfg, pub, log)
	for name, c := range map[string]func() int64{
		"events": r.Stats.Events.Load, "batches": r.Stats.Batches.Load, "duplicates": r.Stats.Dups.Load,
		"delayed": r.Stats.Delayed.Load, "malformed": r.Stats.Bad.Load, "publish_errors": r.Stats.PublishErrors.Load,
	} {
		c := c
		promauto.NewCounterFunc(prometheus.CounterOpts{Name: "simulator_" + name + "_total", Help: "Simulator " + name},
			func() float64 { return float64(c()) })
	}
	promauto.NewGaugeFunc(prometheus.GaugeOpts{Name: "simulator_buffered_events", Help: "Events buffered during simulated outage"},
		func() float64 { return float64(r.Stats.Buffered.Load()) })
	platform.ServeOps(ctx, platform.Env("OPS_ADDR", ":9102"), nil, log, map[string]http.Handler{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var last int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n := r.Stats.Events.Load()
				log.Info("simulator throughput", "events_per_sec", (n-last)/10, "total", n,
					"dups", r.Stats.Dups.Load(), "delayed", r.Stats.Delayed.Load(), "publish_errors", r.Stats.PublishErrors.Load())
				last = n
			}
		}
	}()
	log.Info("streaming", "vehicles", len(w.Vehicles), "interval", cfg.Interval, "transport", platform.Env("SIM_TRANSPORT", "mqtt"))
	return r.Run(ctx)
}
