package ingest

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/oem"
	"fleetpulse/pipeline/internal/sim"
)

type countSink struct{ n, rejected atomic.Int64 }

func (c *countSink) Produce(_ context.Context, e []canonical.Event, r []oem.Reject) error {
	c.n.Add(int64(len(e)))
	c.rejected.Add(int64(len(r)))
	return nil
}
func (c *countSink) Pressure() float64 { return 0 }

// BenchmarkGatewayHandle measures decode + validate + canonicalise for
// realistic 500-record OEM batches (one op = one batch). Kafka produce is
// excluded: it is asynchronous and batched by the client.
func BenchmarkGatewayHandle(b *testing.B) {
	w, err := sim.BuildWorld(42, 3000)
	if err != nil {
		b.Fatal(err)
	}
	payloads := map[string][]byte{}
	for _, code := range []string{"aurora", "pinnacle", "stellar"} {
		enc := sim.EncoderFor(code)
		buf := enc.Begin(nil)
		n := 0
		for _, v := range w.Vehicles {
			if v.OEM != code || n == 500 {
				continue
			}
			l := sim.NewLive(v, 1_790_000_000, 1)
			e := l.Step(1_790_000_001, 1)
			buf = enc.Record(buf, &e, n == 0)
			n++
		}
		payloads[code] = enc.End(buf)
	}
	sink := &countSink{}
	g := NewGateway(oem.DefaultRegistry(), sink, slog.New(slog.NewTextHandler(io.Discard, nil)), 64, "")
	g.Now = func() time.Time { return time.UnixMilli(1_790_000_002_000) }
	codes := []string{"aurora", "pinnacle", "stellar"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := codes[i%3]
		if _, err := g.Handle(context.Background(), c, "bench", payloads[c]); err != nil {
			b.Fatal(err)
		}
	}
	if rej := sink.rejected.Load(); rej > sink.n.Load()/100 {
		b.Fatalf("%d records rejected, %d accepted: benchmark is measuring the error path", rej, sink.n.Load())
	}
	b.ReportMetric(float64(sink.n.Load())/b.Elapsed().Seconds(), "records/s")
}
