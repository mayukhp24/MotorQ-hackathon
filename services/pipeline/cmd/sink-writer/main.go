// Command sink-writer persists alerts and trips from Kafka into PostgreSQL.
package main

import (
	"context"
	"os"
	"time"

	"fleetpulse/pipeline/internal/migrate"
	"fleetpulse/pipeline/internal/platform"
	"fleetpulse/pipeline/internal/sink"
)

func main() {
	log := platform.Logger("sink-writer")
	ctx, cancel := platform.SignalContext()
	defer cancel()
	pool, err := migrate.Connect(ctx, platform.Env("DATABASE_URL", "postgres://fleetpulse_writer@localhost:5432/fleetpulse"), log)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	platform.ServeOps(ctx, platform.Env("OPS_ADDR", ":9104"), func() error {
		c, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return pool.Ping(c)
	}, log, nil)
	if err := sink.Run(ctx, pool, log); err != nil {
		log.Error("sink stopped", "err", err)
		os.Exit(1)
	}
}
