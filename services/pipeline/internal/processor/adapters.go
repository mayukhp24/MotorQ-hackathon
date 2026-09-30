package processor

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"fleetpulse/pipeline/internal/detect"
	"fleetpulse/pipeline/internal/platform"
)

// ---- Kafka output ---------------------------------------------------------

type KafkaOutput struct {
	Client *kgo.Client
	Errors atomic.Int64
}

func (k *KafkaOutput) produce(topic, key string, v []byte) {
	k.Client.Produce(context.Background(), &kgo.Record{Topic: topic, Key: []byte(key), Value: v}, func(_ *kgo.Record, err error) {
		if err != nil {
			k.Errors.Add(1)
		}
	})
}

func (k *KafkaOutput) Enriched(key string, v []byte)   { k.produce(platform.TopicEnriched, key, v) }
func (k *KafkaOutput) Alert(key string, v []byte)      { k.produce(platform.TopicAlerts, key, v) }
func (k *KafkaOutput) Trip(key string, v []byte)       { k.produce(platform.TopicTrips, key, v) }
func (k *KafkaOutput) DeadLetter(key string, v []byte) { k.produce(platform.TopicDLQ, key, v) }

// ---- Redis live store -------------------------------------------------------

type RedisLive struct {
	R *redis.Client
}

func fstr(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', 1, 64)
}

// WriteVehicles pipelines HSET v:{vin} + GEOADD geo:{tenant} in chunks.
func (r *RedisLive) WriteVehicles(ctx context.Context, recs []LiveRecord) error {
	const chunk = 2000
	for i := 0; i < len(recs); i += chunk {
		end := min(i+chunk, len(recs))
		pipe := r.R.Pipeline()
		for _, x := range recs[i:end] {
			ign := "0"
			if x.Ignition {
				ign = "1"
			}
			pipe.HSet(ctx, "v:"+x.VIN,
				"t", x.TenantID, "f", x.FleetID, "st", x.Status, "pt", x.Powertrain, "gh", x.Geohash,
				"lat", strconv.FormatFloat(x.Lat, 'f', 6, 64), "lon", strconv.FormatFloat(x.Lon, 'f', 6, 64),
				"spd", strconv.FormatFloat(x.Speed, 'f', 1, 64), "hdg", strconv.FormatFloat(x.Heading, 'f', 0, 64),
				"odo", strconv.FormatFloat(x.Odo, 'f', 1, 64), "soc", fstr(x.Soc), "fuel", fstr(x.Fuel),
				"cool", fstr(x.Coolant), "bv", fstr(x.BattV), "pack", fstr(x.PackTemp),
				"ts", strconv.FormatInt(x.TsMs, 10), "ig", ign, "al", x.LastAlert, "als", x.LastAlertSev,
				"crit", strconv.FormatInt(x.CriticalUntilMs, 10))
			pipe.GeoAdd(ctx, "geo:"+x.TenantID, &redis.GeoLocation{Name: x.VIN, Longitude: x.Lon, Latitude: x.Lat})
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *RedisLive) PublishAlert(ctx context.Context, tenant string, payload []byte) error {
	return r.R.Publish(ctx, "alerts:"+tenant, payload).Err()
}

func (r *RedisLive) WriteSnapshot(ctx context.Context, key, field string, payload []byte) error {
	pipe := r.R.Pipeline()
	pipe.HSet(ctx, key, field, payload)
	pipe.Expire(ctx, key, 5*time.Minute)
	_, err := pipe.Exec(ctx)
	return err
}

// ---- Postgres vehicle registry -----------------------------------------------

// PGMeta caches VIN → tenant metadata, refreshed periodically so newly
// provisioned vehicles are picked up without a restart. Reads are lock-free.
type PGMeta struct {
	Pool *pgxpool.Pool
	m    atomic.Pointer[map[string]detect.VehicleMeta]
	mu   sync.Mutex
}

const metaQuery = `
SELECT v.vin, v.tenant_id::text, v.fleet_id::text, m.powertrain, COALESCE(a.driver_id::text, '')
FROM vehicle v
JOIN vehicle_model m ON m.model_id = v.model_id
LEFT JOIN vehicle_assignment a ON a.vin = v.vin AND a.valid_to IS NULL
WHERE v.status <> 'DECOMMISSIONED'`

func (p *PGMeta) Load(ctx context.Context) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rows, err := p.Pool.Query(ctx, metaQuery)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	m := make(map[string]detect.VehicleMeta, 120_000)
	for rows.Next() {
		var vin string
		var meta detect.VehicleMeta
		if err := rows.Scan(&vin, &meta.TenantID, &meta.FleetID, &meta.Powertrain, &meta.DriverID); err != nil {
			return 0, err
		}
		m[vin] = meta
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	p.m.Store(&m)
	return len(m), nil
}

func (p *PGMeta) Get(vin string) (detect.VehicleMeta, bool) {
	m := p.m.Load()
	if m == nil {
		return detect.VehicleMeta{}, false
	}
	v, ok := (*m)[vin]
	return v, ok
}

// Refresh reloads on an interval until ctx is done.
func (p *PGMeta) Refresh(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := p.Load(ctx); err != nil {
				log.Warn("vehicle registry refresh failed; keeping previous snapshot", "err", err)
			} else {
				log.Debug("vehicle registry refreshed", "vehicles", n)
			}
		}
	}
}

// LoadWithRetry fails fast after a bounded number of attempts.
func (p *PGMeta) LoadWithRetry(ctx context.Context, attempts int, log *slog.Logger) (int, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		n, err := p.Load(ctx)
		if err == nil && n > 0 {
			return n, nil
		}
		lastErr = err
		if err == nil {
			lastErr = fmt.Errorf("vehicle registry empty (has the database been seeded?)")
		}
		log.Warn("waiting for vehicle registry", "attempt", i, "err", lastErr)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return 0, lastErr
}

// ---- Consumer loop ------------------------------------------------------------

// Run consumes telemetry.v1 with at-least-once semantics: outputs are
// flushed (acked by Kafka) before input offsets are committed. Duplicate
// replays after a crash are absorbed by the Bloom-filter dedup and the
// idempotent sinks (deterministic IDs, ReplacingMergeTree).
func Run(ctx context.Context, p *Processor, group string, log *slog.Logger) error {
	var cl *kgo.Client
	var err error
	cl, err = platform.NewKafka(append(platform.GroupLiveness(),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(platform.TopicTelemetry),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.FetchMaxBytes(32<<20),
		kgo.OnPartitionsRevoked(func(ctx context.Context, c *kgo.Client, m map[string][]int32) {
			if err := c.CommitUncommittedOffsets(ctx); err != nil {
				log.Warn("commit on revoke failed", "err", err)
			}
			p.Revoke(m[platform.TopicTelemetry])
			log.Info("partitions revoked", "partitions", m[platform.TopicTelemetry])
		}),
		kgo.OnPartitionsLost(func(_ context.Context, _ *kgo.Client, m map[string][]int32) {
			p.Revoke(m[platform.TopicTelemetry])
			log.Warn("partitions lost", "partitions", m[platform.TopicTelemetry])
		}),
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, m map[string][]int32) {
			log.Info("partitions assigned", "partitions", m[platform.TopicTelemetry])
		}),
	)...)
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := platform.EnsureTopics(ctx, cl, log); err != nil {
		return err
	}
	out := &KafkaOutput{Client: cl}
	p.out = out
	maxPoll := platform.EnvInt("PROCESSOR_MAX_POLL", 20000)
	for {
		fetches := cl.PollRecords(ctx, maxPoll)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return nil
		}
		fetches.EachError(func(t string, part int32, err error) {
			log.Warn("fetch error", "topic", t, "partition", part, "err", err)
		})
		var wg sync.WaitGroup
		fetches.EachPartition(func(ftp kgo.FetchTopicPartition) {
			if len(ftp.Records) == 0 {
				return
			}
			vals := make([][]byte, len(ftp.Records))
			for i, r := range ftp.Records {
				vals[i] = r.Value
			}
			wg.Add(1)
			go func(part int32) {
				defer wg.Done()
				p.Process(ctx, part, vals)
			}(ftp.Partition)
		})
		wg.Wait()
		if err := cl.Flush(ctx); err != nil {
			log.Warn("flush interrupted", "err", err)
		}
		if err := cl.CommitUncommittedOffsets(ctx); err != nil {
			log.Warn("commit failed", "err", err)
		}
		cl.AllowRebalance()
	}
}
