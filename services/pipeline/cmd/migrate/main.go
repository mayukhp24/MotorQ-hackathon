// Command migrate applies PostgreSQL and ClickHouse schema migrations.
package main

import (
	"os"

	"fleetpulse/pipeline/internal/migrate"
	"fleetpulse/pipeline/internal/platform"
)

func main() {
	log := platform.Logger("migrate")
	ctx, cancel := platform.SignalContext()
	defer cancel()

	pool, err := migrate.Connect(ctx, platform.Env("MIGRATE_DATABASE_URL", "postgres://postgres:postgres@localhost:5432/fleetpulse"), log)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	pw := map[string]string{
		"fleetpulse_api":    os.Getenv("DB_API_PASSWORD"),
		"fleetpulse_writer": os.Getenv("DB_WRITER_PASSWORD"),
	}
	if err := migrate.Postgres(ctx, pool, platform.Env("PG_MIGRATIONS_DIR", "/migrations/postgres"), pw, log); err != nil {
		log.Error("postgres migrations failed", "err", err)
		os.Exit(1)
	}
	// Topics are provisioned here too, so ClickHouse's Kafka engines and the
	// services find them on first start (auto-create is disabled on brokers).
	cl, err := platform.NewKafka()
	if err != nil {
		log.Error("kafka client", "err", err)
		os.Exit(1)
	}
	defer cl.Close()
	if err := platform.EnsureTopics(ctx, cl, log); err != nil {
		log.Error("kafka topics", "err", err)
		os.Exit(1)
	}
	ch := &migrate.ClickHouse{URL: platform.Env("CLICKHOUSE_URL", "http://localhost:8123"),
		User: platform.Env("CLICKHOUSE_USER", "default"), Password: platform.Env("CLICKHOUSE_PASSWORD", "")}
	vars := map[string]string{
		"KAFKA_BROKERS":      platform.Env("KAFKA_BROKERS", "kafka:9092"),
		"CH_KAFKA_CONSUMERS": platform.Env("CH_KAFKA_CONSUMERS", "2"),
		"CH_HOT_DAYS":        platform.Env("CH_HOT_DAYS", "3"),
		"CH_RETENTION_DAYS":  platform.Env("CH_RETENTION_DAYS", "30"),
		"CH_RO_PASSWORD":     platform.Env("CLICKHOUSE_RO_PASSWORD", ""),
	}
	if err := migrate.ClickHouseDir(ctx, ch, platform.Env("CH_MIGRATIONS_DIR", "/migrations/clickhouse"), vars, log); err != nil {
		log.Error("clickhouse migrations failed", "err", err)
		os.Exit(1)
	}
	log.Info("migrations complete")
}
