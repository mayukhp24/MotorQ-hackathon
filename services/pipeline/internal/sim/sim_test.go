package sim

import (
	"context"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/oem"
	"fleetpulse/pipeline/internal/vin"
)

func world(t *testing.T, n int) *World {
	t.Helper()
	w, err := BuildWorld(42, n)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestBuildWorld_DeterministicAndValid(t *testing.T) {
	a, b := world(t, 3000), world(t, 3000)
	if len(a.Vehicles) != 3000 {
		t.Fatalf("vehicles=%d", len(a.Vehicles))
	}
	seen := map[string]bool{}
	evs := 0
	for i, v := range a.Vehicles {
		if v.VIN != b.Vehicles[i].VIN || v.Driver.ID != b.Vehicles[i].Driver.ID {
			t.Fatal("world generation is not deterministic")
		}
		if err := vin.Validate(v.VIN); err != nil {
			t.Fatalf("invalid VIN %s: %v", v.VIN, err)
		}
		if seen[v.VIN] {
			t.Fatalf("duplicate VIN %s", v.VIN)
		}
		seen[v.VIN] = true
		if v.Model.Powertrain != v.Powertrain {
			t.Fatalf("model/powertrain mismatch %s %s", v.Model.Name, v.Powertrain)
		}
		if v.Powertrain == "EV" {
			evs++
		}
		if a.ByVIN(v.VIN) != v {
			t.Fatal("ByVIN index broken")
		}
	}
	if evs == 0 {
		t.Fatal("expected EVs in the mix")
	}
	if len(a.Tenants) != 3 || len(a.Depots) != len(a.Fleets) {
		t.Fatalf("tenants=%d depots=%d fleets=%d", len(a.Tenants), len(a.Depots), len(a.Fleets))
	}
}

func TestEpisodes_BreakdownRatePlausible(t *testing.T) {
	w := world(t, 5000)
	from := float64(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Unix()) / 86400
	broke := 0
	for _, v := range w.Vehicles {
		if len(v.FailuresBetween(from, from+60)) > 0 {
			broke++
		}
		for _, e := range v.Episodes {
			if e.Failure <= e.Onset || e.Failure-e.Onset > 20.01 {
				t.Fatalf("bad episode %+v", e)
			}
		}
	}
	rate := float64(broke) / float64(len(w.Vehicles))
	if rate < 0.04 || rate > 0.30 {
		t.Fatalf("60-day breakdown rate %.3f outside plausible band", rate)
	}
}

func TestHealth_SeverityRampsBeforeFailure(t *testing.T) {
	w := world(t, 5000)
	for _, v := range w.Vehicles {
		for _, e := range v.Episodes {
			if e.Silent {
				continue
			}
			early := v.HealthAt(e.Onset + 0.1*(e.Failure-e.Onset)).Sev[e.Component]
			late := v.HealthAt(e.Onset + 0.9*(e.Failure-e.Onset)).Sev[e.Component]
			if late <= early {
				t.Fatalf("severity should increase towards failure: %v -> %v", early, late)
			}
			if !v.HealthAt(e.Failure + e.RepairDays/2).InRepair {
				t.Fatal("vehicle should be in repair right after failure")
			}
			return
		}
	}
	t.Fatal("no non-silent episode found")
}

func TestDaily_DegradationShowsInFeatures(t *testing.T) {
	w := world(t, 20000)
	var healthyMax, sickMax []float64
	for _, v := range w.Vehicles {
		if v.Powertrain == "EV" {
			continue
		}
		for _, e := range v.Episodes {
			if e.Component != CompCooling || e.Silent {
				continue
			}
			d := int(e.Failure) - 1
			if row, ok := v.Daily(d); ok && row.MaxCoolant != nil {
				sickMax = append(sickMax, *row.MaxCoolant)
			}
			if row, ok := v.Daily(int(e.Onset) - 30); ok && row.MaxCoolant != nil {
				healthyMax = append(healthyMax, *row.MaxCoolant)
			}
		}
	}
	if len(sickMax) < 5 || len(healthyMax) < 5 {
		t.Fatalf("not enough samples %d %d", len(sickMax), len(healthyMax))
	}
	if mean(sickMax) < mean(healthyMax)+8 {
		t.Fatalf("degradation not visible: sick %.1f vs healthy %.1f", mean(sickMax), mean(healthyMax))
	}
}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func TestDaily_RowsTripsAlerts(t *testing.T) {
	w := world(t, 500)
	day := int(float64(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()) / 86400)
	rows, trips, alerts := 0, 0, 0
	for _, v := range w.Vehicles {
		row, ok := v.Daily(day)
		if !ok {
			continue
		}
		rows++
		if row.OdoMax <= row.OdoMin || row.IgnSamples == 0 || row.MinBattV == nil {
			t.Fatalf("bad row %+v", row)
		}
		if v.Powertrain == "EV" && (row.MaxCoolant != nil || row.MinSoc == nil) {
			t.Fatal("EV rows must have SoC and no coolant")
		}
		tr := v.TripsFor(row)
		trips += len(tr)
		for _, x := range tr {
			if x.EndMs <= x.StartMs || x.DistanceKm <= 0 {
				t.Fatalf("bad trip %+v", x)
			}
		}
		alerts += len(v.AlertsFor(row))
		again, _ := v.Daily(day)
		if again.OdoMax != row.OdoMax || again.DTCTotal != row.DTCTotal {
			t.Fatal("daily generation must be deterministic")
		}
	}
	if rows < 250 || trips < rows {
		t.Fatalf("rows=%d trips=%d", rows, trips)
	}
	t.Logf("rows=%d trips=%d alerts=%d", rows, trips, alerts)
}

func TestPoisson(t *testing.T) {
	w := world(t, 1)
	_ = w
	r := newRand(1)
	for _, lambda := range []float64{0.5, 4, 80} {
		s := 0.0
		for i := 0; i < 20000; i++ {
			s += float64(Poisson(r, lambda))
		}
		if m := s / 20000; math.Abs(m-lambda) > 0.05*lambda+0.05 {
			t.Fatalf("lambda %.1f mean %.3f", lambda, m)
		}
	}
	if Poisson(r, 0) != 0 {
		t.Fatal("lambda 0")
	}
}

// Contract test: every simulator encoding must round-trip through the
// ingest adapters to the same canonical values.
func TestEncoders_RoundTripThroughAdapters(t *testing.T) {
	w := world(t, 300)
	reg := oem.DefaultRegistry()
	now := float64(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC).Unix())
	checked := map[string]int{}
	for _, v := range w.Vehicles {
		l := NewLive(v, now, 1)
		for s := 0; s < 20; s++ {
			e := l.Step(now+float64(s), 1)
			e.DTC = []string{"P0301", "P0A80"}
			e.Events = []string{canonical.EvtHarshBrake}
			enc := EncoderFor(v.OEM)
			buf := enc.End(enc.Record(enc.Begin(nil), &e, true))
			a, err := reg.Get(v.OEM)
			if err != nil {
				t.Fatal(err)
			}
			got, rej, err := a.Decode(buf, 7)
			if err != nil || len(rej) > 0 || len(got) != 1 {
				t.Fatalf("%s decode: err=%v rej=%v payload=%s", v.OEM, err, rej, buf)
			}
			g := got[0]
			if err := g.Validate(e.TsMs); err != nil {
				t.Fatalf("%s: decoded event invalid: %v", v.OEM, err)
			}
			if g.VIN != e.VIN || g.Seq != e.Seq || g.TsMs != e.TsMs || g.Ignition != e.Ignition {
				t.Fatalf("%s identity mismatch: %+v vs %+v", v.OEM, g, e)
			}
			near := func(name string, a, b float64, tol float64) {
				if math.Abs(a-b) > tol {
					t.Fatalf("%s %s: %v != %v", v.OEM, name, a, b)
				}
			}
			near("lat", g.Lat, e.Lat, 1e-6)
			near("speed", g.SpeedKmh, e.SpeedKmh, 0.01)
			near("odo", g.OdoKm, e.OdoKm, 0.01)
			near("accel", g.AccelMps2, e.AccelMps2, 0.01)
			if e.CoolantC != nil {
				near("coolant", *g.CoolantC, *e.CoolantC, 0.02)
			}
			if e.SocPct != nil {
				near("soc", *g.SocPct, *e.SocPct, 0.01)
			}
			if e.TyreKpa != nil {
				near("tyre", *g.TyreKpa, *e.TyreKpa, 0.05)
			}
			if len(g.DTC) != 2 || g.DTC[1] != "P0A80" || len(g.Events) != 1 {
				t.Fatalf("%s codes: %v %v", v.OEM, g.DTC, g.Events)
			}
			checked[v.OEM]++
		}
	}
	if len(checked) != 3 {
		t.Fatalf("not all OEMs covered: %v", checked)
	}
}

func TestLive_PhysicalSanity(t *testing.T) {
	w := world(t, 400)
	now := float64(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC).Unix())
	modes := map[string]int{}
	for _, v := range w.Vehicles {
		l := NewLive(v, now, 0.6)
		var lastSeq int64
		for s := 0; s < 300; s++ {
			e := l.Step(now+float64(s), 1)
			if err := e.Validate(e.TsMs); err != nil {
				t.Fatalf("invalid live sample: %v %+v", err, e)
			}
			if e.Seq <= lastSeq {
				t.Fatal("seq must be monotonic")
			}
			lastSeq = e.Seq
			modes[l.Mode()]++
		}
	}
	if modes["DRIVING"] == 0 || modes["PARKED"] == 0 {
		t.Fatalf("expected a mix of modes, got %v", modes)
	}
}

type capturePub struct {
	mu      sync.Mutex
	batches int
	records atomic.Int64
	reg     *oem.Registry
	seen    map[string]int
	fail    int
}

func (c *capturePub) Publish(_ context.Context, o string, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail > 0 {
		c.fail--
		return io.ErrUnexpectedEOF
	}
	a, _ := c.reg.Get(o)
	evs, rej, err := a.Decode(p, 0)
	if err != nil {
		return err
	}
	c.batches++
	c.records.Add(int64(len(evs) + len(rej)))
	for _, e := range evs {
		c.seen[e.EventID]++
	}
	return nil
}

func TestRunner_DeliversWithDuplicatesAndDelays(t *testing.T) {
	w := world(t, 2000)
	cfg := DefaultRunnerConfig()
	cfg.Interval = 200 * time.Millisecond
	cfg.Workers = 2
	cfg.DupRate, cfg.OutOfOrder, cfg.BadRate = 0.05, 0.05, 0.01
	cfg.Limit = 10000
	pub := &capturePub{reg: oem.DefaultRegistry(), seen: map[string]int{}, fail: 1}
	r := NewRunner(w, cfg, pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if r.Stats.Events.Load() < 10000 {
		t.Fatalf("events=%d", r.Stats.Events.Load())
	}
	dups := 0
	for _, n := range pub.seen {
		if n > 1 {
			dups++
		}
	}
	if dups == 0 || r.Stats.Delayed.Load() == 0 || r.Stats.Bad.Load() == 0 || r.Stats.PublishErrors.Load() == 0 {
		t.Fatalf("expected dups/delays/bad/errors: dups=%d delayed=%d bad=%d errs=%d",
			dups, r.Stats.Delayed.Load(), r.Stats.Bad.Load(), r.Stats.PublishErrors.Load())
	}
}

func TestInWindow(t *testing.T) {
	s := time.Unix(0, 0)
	if !inWindow(s, s.Add(30*time.Second), time.Minute, 40*time.Second) || inWindow(s, s.Add(10*time.Second), time.Minute, 40*time.Second) {
		t.Fatal("window math wrong")
	}
	if inWindow(s, s, 0, time.Second) {
		t.Fatal("disabled window")
	}
}
