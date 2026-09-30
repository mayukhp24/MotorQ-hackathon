package processor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	json "github.com/goccy/go-json"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/detect"
	"fleetpulse/pipeline/internal/sim"
)

type nopOut struct{}

func (nopOut) Enriched(string, []byte)   {}
func (nopOut) Alert(string, []byte)      {}
func (nopOut) Trip(string, []byte)       {}
func (nopOut) DeadLetter(string, []byte) {}

// BenchmarkProcess measures the full per-event path of the stream processor
// on one core: JSON decode, Bloom-filter dedup, enrichment, detection rules,
// trip segmentation, live-state staging and enriched re-encode. 5,000
// vehicles each report once per simulated second.
func BenchmarkProcess(b *testing.B) {
	w, err := sim.BuildWorld(42, 5000)
	if err != nil {
		b.Fatal(err)
	}
	meta := fakeMeta{}
	lives := make([]*sim.Live, len(w.Vehicles))
	for i, v := range w.Vehicles {
		meta[v.VIN] = detect.VehicleMeta{TenantID: v.Tenant.ID, FleetID: v.Fleet.ID, Powertrain: v.Powertrain}
		lives[i] = sim.NewLive(v, 1_790_000_000, 0.8)
	}
	const secs = 20
	batches := make([][][]byte, secs)
	for s := 0; s < secs; s++ {
		for _, l := range lives {
			e := l.Step(float64(1_790_000_001+s), 1)
			e.EventID = canonical.ID(e.OEM, e.VIN, e.Seq) // assigned by the gateway adapter in production
			e.IngestMs = e.TsMs + 20
			raw, _ := json.Marshal(e)
			batches[s] = append(batches[s], raw)
		}
	}
	clock := time.UnixMilli(1_790_000_000_000)
	p := New(DefaultConfig(), detect.NewEngine(detect.DefaultThresholds()), meta, nopOut{}, &fakeLive{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.now = func() time.Time { return clock }
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	var processed int64
	for i := 0; i < b.N; i++ {
		s := i % secs
		if s == 0 && i > 0 {
			b.StopTimer() // new epoch: fresh state so dedup does not drop replays
			processed += p.Events()
			p = New(DefaultConfig(), detect.NewEngine(detect.DefaultThresholds()), meta, nopOut{}, &fakeLive{},
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			p.now = func() time.Time { return clock }
			b.StartTimer()
		}
		clock = time.UnixMilli(int64(1_790_000_001+s)*1000 + 50)
		p.Process(ctx, 0, batches[s])
	}
	processed += p.Events()
	if want := int64(b.N) * int64(len(lives)); processed < want*95/100 {
		b.Fatalf("only %d of %d events processed", processed, want)
	}
	b.ReportMetric(float64(processed)/b.Elapsed().Seconds(), "events/s")
}
