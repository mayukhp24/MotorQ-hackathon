// Package platform holds cross-cutting infrastructure helpers: 12-factor
// config from the environment, structured logging, Prometheus metrics and
// Kafka client construction.
package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Env returns the environment variable or a default.
func Env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func EnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func EnvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func EnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Logger returns a JSON slog logger tagged with the service name.
func Logger(service string) *slog.Logger {
	lvl := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})).With("service", service)
}

// SignalContext is cancelled on SIGINT/SIGTERM (graceful shutdown, 12-factor
// disposability).
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ServeOps starts the operational HTTP server (/metrics, /healthz, /readyz)
// plus any extra handlers.
func ServeOps(ctx context.Context, addr string, ready func() error, log *slog.Logger, extra map[string]http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready != nil {
			if err := ready(); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ready"))
	})
	for p, h := range extra {
		mux.Handle(p, h)
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("ops server failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return srv
}

// Topic names and defaults (contracts/README.md).
const (
	TopicTelemetry = "telemetry.v1"
	TopicEnriched  = "telemetry.enriched.v1"
	TopicAlerts    = "alerts.v1"
	TopicTrips     = "trips.v1"
	TopicDLQ       = "telemetry.dlq.v1"
)

// Brokers parses KAFKA_BROKERS.
func Brokers() []string {
	return strings.Split(Env("KAFKA_BROKERS", "localhost:9092"), ",")
}

// NewKafka builds a franz-go client with production defaults: idempotent
// producer, acks=all, lz4 compression, bounded buffering for back-pressure.
func NewKafka(extra ...kgo.Opt) (*kgo.Client, error) {
	opts := []kgo.Opt{
		kgo.SeedBrokers(Brokers()...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.Lz4Compression()),
		kgo.ProducerLinger(time.Duration(EnvInt("KAFKA_LINGER_MS", 5)) * time.Millisecond),
		kgo.ProducerBatchMaxBytes(1 << 20),
		kgo.MaxBufferedRecords(EnvInt("KAFKA_MAX_BUFFERED", 200_000)),
		kgo.RecordDeliveryTimeout(2 * time.Minute),
		kgo.ClientID(Env("SERVICE_NAME", "fleetpulse")),
	}
	return kgo.NewClient(append(opts, extra...)...)
}

// EnsureTopics creates topics idempotently (safe to call from every replica).
func EnsureTopics(ctx context.Context, cl *kgo.Client, log *slog.Logger) error {
	adm := kadm.NewClient(cl)
	rf := int16(EnvInt("KAFKA_REPLICATION", 1))
	parts := int32(EnvInt("TELEMETRY_PARTITIONS", 12))
	// Time- and size-bounded retention (whichever hits first); small segments
	// so size-based deletion can actually reclaim space.
	retBytes := strconv.Itoa(EnvInt("TELEMETRY_RETENTION_BYTES", 1<<30))
	segBytes := strconv.Itoa(64 << 20)
	retention := func(h int) map[string]*string {
		v := strconv.Itoa(h * 3600 * 1000)
		return map[string]*string{"retention.ms": &v, "retention.bytes": &retBytes, "segment.bytes": &segBytes}
	}
	specs := []struct {
		name  string
		parts int32
		cfg   map[string]*string
	}{
		{TopicTelemetry, parts, retention(EnvInt("TELEMETRY_RETENTION_H", 24))},
		{TopicEnriched, parts, retention(EnvInt("TELEMETRY_RETENTION_H", 24))},
		{TopicAlerts, 6, retention(24 * 7)},
		{TopicTrips, 6, retention(24 * 7)},
		{TopicDLQ, 3, retention(24 * 7)},
	}
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		lastErr = nil
		for _, s := range specs {
			resp, err := adm.CreateTopic(ctx, s.parts, rf, s.cfg, s.name)
			if err == nil {
				err = resp.Err
			}
			if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
				lastErr = err
				break
			}
		}
		if lastErr == nil {
			return nil
		}
		log.Warn("waiting for kafka", "attempt", attempt, "err", lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("ensure topics: %w", lastErr)
}
