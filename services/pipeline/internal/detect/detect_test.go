package detect

import (
	"testing"

	"fleetpulse/pipeline/internal/canonical"
)

const vinA = "1HGCM82633A004352"

var meta = VehicleMeta{TenantID: "t1", FleetID: "f1", DriverID: "d1", Powertrain: "ICE"}

type seq struct {
	t   *testing.T
	g   *Engine
	st  *State
	ts  int64
	n   int64
	odo float64
	all []Alert
	trs []Trip
}

func newSeq(t *testing.T) *seq {
	return &seq{t: t, g: NewEngine(DefaultThresholds()), st: NewState(), ts: 1_790_000_000_000, odo: 1000}
}

// step emits one sample 1 s after the previous one.
func (s *seq) step(mut func(e *canonical.Event)) Result {
	s.ts += 1000
	s.n++
	e := canonical.Event{VIN: vinA, OEM: "aurora", TsMs: s.ts, Seq: s.n, Ignition: true,
		SpeedKmh: 50, OdoKm: s.odo, Lat: 12.9, Lon: 77.6, CoolantC: canonical.F(90), BattV: canonical.F(13.8)}
	if mut != nil {
		mut(&e)
	}
	s.odo = e.OdoKm + e.SpeedKmh/3600
	r := s.g.Process(s.st, &e, meta, s.ts+50)
	s.all = append(s.all, r.Alerts...)
	if r.Trip != nil {
		s.trs = append(s.trs, *r.Trip)
	}
	return r
}

func (s *seq) count(typ string) int {
	n := 0
	for _, a := range s.all {
		if a.Type == typ {
			n++
		}
	}
	return n
}

func TestOverheat_RequiresSustainedTemperature(t *testing.T) {
	s := newSeq(t)
	// A 5 s spike must not alert.
	for i := 0; i < 5; i++ {
		s.step(func(e *canonical.Event) { e.CoolantC = canonical.F(115) })
	}
	s.step(nil)
	if s.count(AlertOverheat) != 0 {
		t.Fatal("spike should not alert")
	}
	for i := 0; i < 20; i++ {
		s.step(func(e *canonical.Event) { e.CoolantC = canonical.F(115) })
	}
	if s.count(AlertOverheat) != 1 {
		t.Fatalf("want exactly 1 overheat alert (cooldown), got %d", s.count(AlertOverheat))
	}
	a := s.all[len(s.all)-1]
	if a.Severity != SevCritical || a.TenantID != "t1" || a.AlertID == "" || a.DetectedMs <= a.TsMs {
		t.Fatalf("bad alert %+v", a)
	}
}

func TestCoolantHigh_Precursor(t *testing.T) {
	s := newSeq(t)
	for i := 0; i < 70; i++ {
		s.step(func(e *canonical.Event) { e.CoolantC = canonical.F(106) })
	}
	if s.count(AlertCoolantHigh) != 1 || s.count(AlertOverheat) != 0 {
		t.Fatalf("high=%d overheat=%d", s.count(AlertCoolantHigh), s.count(AlertOverheat))
	}
}

func TestDTC_NewCodesOnly(t *testing.T) {
	s := newSeq(t)
	for i := 0; i < 10; i++ {
		s.step(func(e *canonical.Event) { e.DTC = []string{"P0217", "P0442", "P0301"} })
	}
	if s.count(AlertCriticalDTC) != 1 {
		t.Fatalf("critical dtc alerts = %d", s.count(AlertCriticalDTC))
	}
	if s.count(AlertMajorDTC) != 1 {
		t.Fatalf("major dtc alerts = %d", s.count(AlertMajorDTC))
	}
	s.step(nil) // code clears
	s.step(func(e *canonical.Event) { e.DTC = []string{"P0217"} })
	if s.count(AlertCriticalDTC) != 1 {
		t.Fatal("re-appearing code inside cooldown must not re-alert")
	}
}

func TestLowVoltage_OnlyWithIgnition(t *testing.T) {
	s := newSeq(t)
	for i := 0; i < 40; i++ {
		s.step(func(e *canonical.Event) { e.BattV = canonical.F(11.2); e.Ignition = false; e.SpeedKmh = 0 })
	}
	if s.count(AlertLowVoltage) != 0 {
		t.Fatal("parked vehicle should not alert")
	}
	for i := 0; i < 40; i++ {
		s.step(func(e *canonical.Event) { e.BattV = canonical.F(11.2) })
	}
	if s.count(AlertLowVoltage) != 1 {
		t.Fatal("expected low voltage alert")
	}
}

func TestEV_PackAndSoC(t *testing.T) {
	s := newSeq(t)
	s.step(func(e *canonical.Event) {
		e.CoolantC = nil
		e.PackTempC = canonical.F(58)
		e.SocPct = canonical.F(7)
	})
	if s.count(AlertPackOverTemp) != 1 || s.count(AlertLowSoC) != 1 {
		t.Fatalf("pack=%d soc=%d", s.count(AlertPackOverTemp), s.count(AlertLowSoC))
	}
	s2 := newSeq(t)
	s2.step(func(e *canonical.Event) { e.SocPct = canonical.F(7); e.Charging = true })
	if s2.count(AlertLowSoC) != 0 || s2.st.Status != StatusCharging {
		t.Fatal("charging vehicle should not raise low SoC")
	}
}

func TestRiskyDriving_DerivedDeceleration(t *testing.T) {
	s := newSeq(t)
	speeds := []float64{80, 50, 80, 50, 80, 50}
	for _, v := range speeds {
		v := v
		s.step(func(e *canonical.Event) { e.SpeedKmh = v })
	}
	// 30 km/h drop in 1 s = -8.3 m/s² three times.
	if s.count(AlertRiskyDriving) != 1 {
		t.Fatalf("risky = %d", s.count(AlertRiskyDriving))
	}
}

func TestCrashAndTyreAndOverspeed(t *testing.T) {
	s := newSeq(t)
	s.step(func(e *canonical.Event) { e.Events = []string{canonical.EvtCrash}; e.TyreKpa = canonical.F(150) })
	for i := 0; i < 35; i++ {
		s.step(func(e *canonical.Event) { e.SpeedKmh = 130 })
	}
	for _, typ := range []string{AlertCrash, AlertTyrePressure, AlertOverspeed} {
		if s.count(typ) != 1 {
			t.Errorf("%s count = %d", typ, s.count(typ))
		}
	}
}

func TestIdle_CostEstimate(t *testing.T) {
	s := newSeq(t)
	for i := 0; i < 11*60; i++ {
		s.step(func(e *canonical.Event) { e.SpeedKmh = 0 })
	}
	if s.count(AlertExcessiveIdle) != 1 {
		t.Fatalf("idle alerts = %d", s.count(AlertExcessiveIdle))
	}
	var a Alert
	for _, x := range s.all {
		if x.Type == AlertExcessiveIdle {
			a = x
		}
	}
	if c, _ := a.Details["est_cost_usd"].(float64); c <= 0 {
		t.Fatalf("expected positive cost, got %v", a.Details)
	}
	if s.st.Status != StatusIdling {
		t.Fatal("status should be IDLING")
	}
}

func TestLateEventsIgnoredForState(t *testing.T) {
	s := newSeq(t)
	s.step(nil)
	s.step(nil)
	late := canonical.Event{VIN: vinA, TsMs: s.ts - 5000, Seq: 1, CoolantC: canonical.F(150), Ignition: true}
	r := s.g.Process(s.st, &late, meta, s.ts)
	if !r.Late || len(r.Alerts) != 0 || s.st.LateEvents != 1 {
		t.Fatalf("late event mishandled: %+v", r)
	}
}

func TestTripSegmentation(t *testing.T) {
	s := newSeq(t)
	s.step(func(e *canonical.Event) { e.Ignition = false; e.SpeedKmh = 0 })
	for i := 0; i < 600; i++ { // 10 minutes at 60 km/h ≈ 10 km
		s.step(func(e *canonical.Event) { e.SpeedKmh = 60; e.FuelPct = canonical.F(50 - float64(i)/100) })
	}
	for i := 0; i < 30; i++ {
		s.step(func(e *canonical.Event) { e.SpeedKmh = 0 })
	}
	s.step(func(e *canonical.Event) { e.Ignition = false; e.SpeedKmh = 0 })
	if len(s.trs) != 1 {
		t.Fatalf("trips = %d", len(s.trs))
	}
	tr := s.trs[0]
	if tr.DistanceKm < 9.5 || tr.DistanceKm > 10.5 {
		t.Fatalf("distance %.2f", tr.DistanceKm)
	}
	if tr.IdleSec < 29 || tr.IdleSec > 31 || tr.MaxSpeedKmh != 60 || tr.DriverID != "d1" || tr.EnergyUsed <= 5 {
		t.Fatalf("bad trip %+v", tr)
	}
	// Short hops below MinTripKm are discarded.
	s.step(func(e *canonical.Event) { e.SpeedKmh = 10 })
	s.step(func(e *canonical.Event) { e.Ignition = false; e.SpeedKmh = 0 })
	if len(s.trs) != 1 {
		t.Fatal("tiny trip should be discarded")
	}
}

func TestAlertIDDeterministic(t *testing.T) {
	if alertID(vinA, "X", 1) != alertID(vinA, "X", 1) || alertID(vinA, "X", 1) == alertID(vinA, "X", 2) {
		t.Fatal("alert id must be deterministic per bucket")
	}
}

func TestEVIdleCostUsesPowerPrice(t *testing.T) {
	g := NewEngine(DefaultThresholds())
	st := NewState()
	ts := int64(1_790_000_000_000)
	var got []Alert
	for i := 0; i < 11*60; i++ {
		ts += 1000
		e := canonical.Event{VIN: vinA, TsMs: ts, Seq: int64(i), Ignition: true, SocPct: canonical.F(60)}
		got = append(got, g.Process(st, &e, VehicleMeta{Powertrain: "EV"}, ts).Alerts...)
	}
	if len(got) != 1 {
		t.Fatalf("alerts=%d", len(got))
	}
	if c := got[0].Details["est_cost_usd"].(float64); c > 0.2 {
		t.Fatalf("EV idle cost should be small, got %v", c)
	}
}
