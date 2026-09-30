package processor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	json "github.com/goccy/go-json"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/detect"
)

type fakeMeta map[string]detect.VehicleMeta

func (f fakeMeta) Get(v string) (detect.VehicleMeta, bool) { m, ok := f[v]; return m, ok }

type fakeOut struct {
	mu                          sync.Mutex
	enriched, alerts, trips, dl [][]byte
}

func (f *fakeOut) Enriched(_ string, v []byte) {
	f.mu.Lock()
	f.enriched = append(f.enriched, v)
	f.mu.Unlock()
}
func (f *fakeOut) Alert(_ string, v []byte) {
	f.mu.Lock()
	f.alerts = append(f.alerts, v)
	f.mu.Unlock()
}
func (f *fakeOut) Trip(_ string, v []byte)       { f.mu.Lock(); f.trips = append(f.trips, v); f.mu.Unlock() }
func (f *fakeOut) DeadLetter(_ string, v []byte) { f.mu.Lock(); f.dl = append(f.dl, v); f.mu.Unlock() }

type fakeLive struct {
	mu        sync.Mutex
	recs      []LiveRecord
	published int
	snaps     map[string]map[string][]byte
}

func (f *fakeLive) WriteVehicles(_ context.Context, r []LiveRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r...)
	return nil
}
func (f *fakeLive) PublishAlert(context.Context, string, []byte) error {
	f.mu.Lock()
	f.published++
	f.mu.Unlock()
	return nil
}
func (f *fakeLive) WriteSnapshot(_ context.Context, k, field string, v []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snaps == nil {
		f.snaps = map[string]map[string][]byte{}
	}
	if f.snaps[k] == nil {
		f.snaps[k] = map[string][]byte{}
	}
	f.snaps[k][field] = v
	return nil
}

const vinA = "1HGCM82633A004352"

func setup() (*Processor, *fakeOut, *fakeLive, *time.Time) {
	clock := time.UnixMilli(1_790_000_000_000)
	out, live := &fakeOut{}, &fakeLive{}
	meta := fakeMeta{vinA: {TenantID: "t1", FleetID: "f1", Powertrain: "ICE"}}
	p := New(DefaultConfig(), detect.NewEngine(detect.DefaultThresholds()), meta, out, live,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.cfg.DedupPerPart = 10000
	p.now = func() time.Time { return clock }
	return p, out, live, &clock
}

func ev(seq int64, ts int64, mut func(*canonical.Event)) []byte {
	e := canonical.Event{EventID: canonical.ID("aurora", vinA, seq), VIN: vinA, OEM: "aurora", TsMs: ts, Seq: seq,
		Lat: 12.97, Lon: 77.59, SpeedKmh: 40, Ignition: true, CoolantC: canonical.F(90), IngestMs: ts + 20}
	if mut != nil {
		mut(&e)
	}
	b, _ := json.Marshal(e)
	return b
}

func TestProcess_DedupEnrichAndAlert(t *testing.T) {
	p, out, live, clock := setup()
	base := clock.UnixMilli()
	var batch [][]byte
	for i := int64(1); i <= 30; i++ {
		batch = append(batch, ev(i, base+i*1000, func(e *canonical.Event) { e.CoolantC = canonical.F(115) }))
	}
	batch = append(batch, batch[3], batch[4]) // duplicates
	*clock = clock.Add(31 * time.Second)
	p.Process(context.Background(), 0, batch)
	if len(out.enriched) != 30 {
		t.Fatalf("enriched=%d (dups must be dropped)", len(out.enriched))
	}
	var enr canonical.Enriched
	_ = json.Unmarshal(out.enriched[0], &enr)
	if enr.TenantID != "t1" || len(enr.Geohash) != 7 || enr.ProcMs == 0 {
		t.Fatalf("enrichment wrong: %+v", enr)
	}
	if len(out.alerts) != 1 || live.published != 1 {
		t.Fatalf("alerts=%d published=%d", len(out.alerts), live.published)
	}
	if !strings.Contains(string(out.alerts[0]), detect.AlertOverheat) {
		t.Fatalf("alert=%s", out.alerts[0])
	}
}

func TestProcess_UnknownVehicleAndGarbage(t *testing.T) {
	p, out, _, clock := setup()
	unknown := ev(1, clock.UnixMilli(), func(e *canonical.Event) { e.VIN = "11111111111111111"; e.EventID = "x" })
	p.Process(context.Background(), 1, [][]byte{unknown, []byte("{garbage")})
	if len(out.dl) != 2 || len(out.enriched) != 0 {
		t.Fatalf("dl=%d enriched=%d", len(out.dl), len(out.enriched))
	}
}

func TestFlushLive_ThrottlesAndWrites(t *testing.T) {
	p, _, live, clock := setup()
	base := clock.UnixMilli()
	p.Process(context.Background(), 0, [][]byte{ev(1, base, nil)})
	if err := p.FlushLive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(live.recs) != 1 || live.recs[0].Status != detect.StatusDriving || live.recs[0].TenantID != "t1" {
		t.Fatalf("recs=%+v", live.recs)
	}
	// Second event within 1 s: throttled.
	*clock = clock.Add(200 * time.Millisecond)
	p.Process(context.Background(), 0, [][]byte{ev(2, base+200, nil)})
	_ = p.FlushLive(context.Background())
	if len(live.recs) != 1 {
		t.Fatalf("expected throttle, got %d writes", len(live.recs))
	}
	*clock = clock.Add(time.Second)
	_ = p.FlushLive(context.Background())
	if len(live.recs) != 2 {
		t.Fatalf("expected write after interval, got %d", len(live.recs))
	}
	// Status change bypasses throttle.
	p.Process(context.Background(), 0, [][]byte{ev(3, base+1300, func(e *canonical.Event) { e.Ignition = false; e.SpeedKmh = 0 })})
	_ = p.FlushLive(context.Background())
	if len(live.recs) != 3 || live.recs[2].Status != detect.StatusParked {
		t.Fatalf("status change not flushed: %+v", live.recs)
	}
}

func TestSnapshot_KPIClustersTopK(t *testing.T) {
	p, _, live, clock := setup()
	base := clock.UnixMilli()
	p.Process(context.Background(), 2, [][]byte{
		ev(1, base, func(e *canonical.Event) { e.DTC = []string{"P0301"} }),
		ev(2, base+1000, func(e *canonical.Event) { e.DTC = []string{"P0301", "P0217"} }),
	})
	*clock = clock.Add(2 * time.Second)
	if err := p.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	var k KPI
	if err := json.Unmarshal(live.snaps["kpi:t1"]["2"], &k); err != nil {
		t.Fatal(err)
	}
	if k.Online != 1 || k.ByStatus[detect.StatusDriving] != 1 || k.Critical != 1 || k.EPS <= 0 {
		t.Fatalf("kpi=%+v", k)
	}
	if _, ok := live.snaps["clu:t1:5"]["2"]; !ok {
		t.Fatal("cluster snapshot missing")
	}
	if !strings.Contains(string(live.snaps["topdtc:t1"]["2"]), "P0301") {
		t.Fatalf("topk=%s", live.snaps["topdtc:t1"]["2"])
	}
	if got := p.Owned(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("owned=%v", got)
	}
	p.Revoke([]int32{2})
	if len(p.Owned()) != 0 {
		t.Fatal("revoke did not drop state")
	}
}

func TestLateEventNotWrittenToLiveState(t *testing.T) {
	p, out, live, clock := setup()
	base := clock.UnixMilli()
	p.Process(context.Background(), 0, [][]byte{ev(2, base, nil), ev(1, base-5000, func(e *canonical.Event) { e.Lat = 1 })})
	_ = p.FlushLive(context.Background())
	if len(out.enriched) != 2 {
		t.Fatal("late events must still reach analytics")
	}
	if live.recs[0].Lat == 1 {
		t.Fatal("late event overwrote live position")
	}
}
