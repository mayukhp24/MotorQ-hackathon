// Package sink persists derived stream records (alerts, trips) into the
// relational store. Writes are idempotent (deterministic IDs + ON CONFLICT
// DO NOTHING), so at-least-once delivery never creates duplicates.
package sink

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kgo"

	"fleetpulse/pipeline/internal/detect"
	"fleetpulse/pipeline/internal/platform"
	"fleetpulse/pipeline/internal/stream"
)

var (
	mRows = promauto.NewCounterVec(prometheus.CounterOpts{Name: "sink_rows_total", Help: "Rows written"}, []string{"table"})
	mErrs = promauto.NewCounterVec(prometheus.CounterOpts{Name: "sink_errors_total", Help: "Write failures"}, []string{"table"})
	mLag  = promauto.NewHistogram(prometheus.HistogramOpts{Name: "sink_alert_persist_seconds", Help: "Vehicle timestamp to alert persisted", Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30}})
)

// DB is the subset of pgx used (a pool in production, fakeable in tests).
type DB interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

const insertAlert = `INSERT INTO alert (alert_id, tenant_id, vin, alert_type, severity, title, status, ts, detected_at, lat, lon, details)
VALUES ($1, $2, $3, $4, $5, $6, 'OPEN', $7, $8, $9, $10, $11) ON CONFLICT (alert_id) DO NOTHING`

const insertTrip = `INSERT INTO trip (trip_id, tenant_id, vin, driver_id, start_ts, end_ts, distance_km, max_speed_kmh, idle_s,
  harsh_brakes, harsh_accels, overspeed_s, energy_used_pct, start_geohash, end_geohash)
VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) ON CONFLICT (trip_id, start_ts) DO NOTHING`

// BuildBatch converts raw Kafka records into one pgx batch. Undecodable
// records are skipped and counted (they are already in the topic for replay).
func BuildBatch(records []*kgo.Record) (*pgx.Batch, int, int) {
	b := &pgx.Batch{}
	alerts, trips := 0, 0
	for _, r := range records {
		switch r.Topic {
		case platform.TopicAlerts:
			var a detect.Alert
			if err := json.Unmarshal(r.Value, &a); err != nil {
				mErrs.WithLabelValues("alert_decode").Inc()
				continue
			}
			details, _ := json.Marshal(a.Details)
			b.Queue(insertAlert, a.AlertID, a.TenantID, a.VIN, a.Type, a.Severity, a.Title,
				time.UnixMilli(a.TsMs).UTC(), time.UnixMilli(a.DetectedMs).UTC(), a.Lat, a.Lon, string(details))
			alerts++
			mLag.Observe(time.Since(time.UnixMilli(a.TsMs)).Seconds())
		case platform.TopicTrips:
			var t detect.Trip
			if err := json.Unmarshal(r.Value, &t); err != nil {
				mErrs.WithLabelValues("trip_decode").Inc()
				continue
			}
			// Data minimisation: trips keep ~1 km geohash cells, not raw GPS.
			b.Queue(insertTrip, t.TripID, t.TenantID, t.VIN, t.DriverID, time.UnixMilli(t.StartMs).UTC(),
				time.UnixMilli(t.EndMs).UTC(), t.DistanceKm, t.MaxSpeedKmh, t.IdleSec, t.HarshBrakes,
				t.HarshAccels, t.OverspeedSec, t.EnergyUsed,
				stream.Geohash(t.StartLat, t.StartLon, 6), stream.Geohash(t.EndLat, t.EndLon, 6))
			trips++
		}
	}
	return b, alerts, trips
}

// Write executes the batch; any failure fails the whole poll so offsets are
// not committed and the records are retried.
func Write(ctx context.Context, db DB, b *pgx.Batch) error {
	if b.Len() == 0 {
		return nil
	}
	br := db.SendBatch(ctx, b)
	defer br.Close()
	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("batch item %d: %w", i, err)
		}
	}
	return nil
}

// Run consumes alerts and trips until ctx is done.
func Run(ctx context.Context, db DB, log *slog.Logger) error {
	cl, err := platform.NewKafka(
		kgo.ConsumerGroup(platform.Env("SINK_GROUP", "sink-writer")),
		kgo.ConsumeTopics(platform.TopicAlerts, platform.TopicTrips),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := platform.EnsureTopics(ctx, cl, log); err != nil {
		return err
	}
	backoff := time.Second
	for {
		fetches := cl.PollRecords(ctx, 5000)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return nil
		}
		recs := fetches.Records()
		if len(recs) == 0 {
			cl.AllowRebalance()
			continue
		}
		b, na, nt := BuildBatch(recs)
		for {
			err := Write(ctx, db, b)
			if err == nil {
				break
			}
			mErrs.WithLabelValues("write").Inc()
			log.Warn("postgres write failed; retrying (offsets not committed)", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			b, na, nt = BuildBatch(recs)
		}
		backoff = time.Second
		mRows.WithLabelValues("alert").Add(float64(na))
		mRows.WithLabelValues("trip").Add(float64(nt))
		if err := cl.CommitRecords(ctx, recs...); err != nil {
			log.Warn("commit failed", "err", err)
		}
		cl.AllowRebalance()
	}
}
