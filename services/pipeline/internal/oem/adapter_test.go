package oem

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"fleetpulse/pipeline/internal/canonical"
)

const testVIN = "1HGCM82633A004352"

var auroraBatch = []byte(`{"oem":"aurora","records":[
 {"vehicle":{"vin":"1HGCM82633A004352"},"timestamp":1790000000123,"sequence":88412,
  "position":{"latitude":21.1702,"longitude":72.8311,"heading":90},
  "motion":{"speedKph":64.2,"accelMps2":-4.1},
  "powertrain":{"odometerKm":18234.7,"engineOn":true,"rpm":2100,"coolantTempC":91.5,"engineLoadPct":40,"fuelLevelPct":55},
  "electrical":{"batteryVoltage":12.6},"chassis":{"tyreMinKpa":230},
  "diagnostics":{"dtcs":["p0301","junk"]},"events":["HARSH_BRAKE","WHEELIE"]},
 {"vehicle":{"vin":"1HGCM82633A004352"},"timestamp":1790000001123,"sequence":88413,
  "position":{"latitude":21.1703},"motion":{"speedKph":60},"powertrain":{"odometerKm":18234.72,"engineOn":true}}
]}`)

var pinnacleBatch = []byte(`{"data":[
 {"VIN":"1hgcm82633a004352","EventTime":"2026-09-25T10:15:02.120Z","Seq":7,"Lat":12.97,"Lng":77.59,"Hdg":180,
  "SpeedMph":40,"AccelG":-0.5,"OdometerMi":1000,"Ign":"ON","Chg":"N","RPM":1500,"CoolantF":212,"LoadPct":30,"FuelPct":40,
  "BattV":12.1,"TireMinPsi":30,"DTC":"P0217, P0128","Evt":"HB|HA|ZZ"}
]}`)

var stellarBatch = []byte(`{"messages":[
 {"vin":"1HGCM82633A004352","t":1790000000.5,"n":42,
  "signals":[{"name":"gps.lat","value":13.08},{"name":"gps.lon","value":80.27},{"name":"gps.heading","value":10},
   {"name":"speed","value":10,"unit":"m/s"},{"name":"accel.long","value":1.2},{"name":"odometer","value":123456,"unit":"m"},
   {"name":"ignition","value":1},{"name":"hv.soc","value":55},{"name":"hv.temp","value":38},{"name":"hv.charging","value":0},
   {"name":"lv.voltage","value":12.4},{"name":"coolant.temp","value":363.15,"unit":"K"},{"name":"rpm","value":0},
   {"name":"engine.load","value":10},{"name":"fuel.level","value":0},{"name":"tpms.min","value":220}],
  "faults":[{"type":"P","code":"0A80"},{"type":"Z","code":"9999"}],
  "alerts":["harshBraking","unknown"]},
 {"vin":"1HGCM82633A004352","t":1790000001.5,"n":43,"signals":[{"name":"gps.lat","value":13.08}]}
]}`)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestAurora(t *testing.T) {
	a, err := DefaultRegistry().Get("aurora")
	if err != nil {
		t.Fatal(err)
	}
	evs, rej, err := a.Decode(auroraBatch, 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || len(rej) != 1 {
		t.Fatalf("got %d events %d rejects", len(evs), len(rej))
	}
	e := evs[0]
	if e.VIN != testVIN || e.TsMs != 1790000000123 || e.Seq != 88412 || e.IngestMs != 99 {
		t.Fatalf("bad identity fields %+v", e)
	}
	if !approx(e.SpeedKmh, 64.2) || !approx(*e.CoolantC, 91.5) || !e.Ignition || *e.TyreKpa != 230 {
		t.Fatalf("bad signals %+v", e)
	}
	if len(e.DTC) != 1 || e.DTC[0] != "P0301" {
		t.Fatalf("dtc = %v", e.DTC)
	}
	if len(e.Events) != 1 || e.Events[0] != canonical.EvtHarshBrake {
		t.Fatalf("events = %v (unknown labels must be dropped)", e.Events)
	}
	if e.SocPct != nil {
		t.Fatal("soc should be nil when not reported")
	}
	if e.EventID != "aurora:"+testVIN+":88412" {
		t.Fatalf("event id %s", e.EventID)
	}
	if err := e.Validate(1790000000123); err != nil {
		t.Fatal(err)
	}
}

func TestPinnacle_UnitConversion(t *testing.T) {
	a, _ := DefaultRegistry().Get("pinnacle")
	evs, rej, err := a.Decode(pinnacleBatch, 1)
	if err != nil || len(rej) != 0 || len(evs) != 1 {
		t.Fatalf("err=%v rej=%v n=%d", err, rej, len(evs))
	}
	e := evs[0]
	if e.VIN != testVIN {
		t.Fatalf("vin not upper-cased: %s", e.VIN)
	}
	if !approx(e.SpeedKmh, 64.37376) || !approx(e.OdoKm, 1609.344) || !approx(*e.CoolantC, 100) {
		t.Fatalf("unit conversion wrong: speed=%v odo=%v coolant=%v", e.SpeedKmh, e.OdoKm, *e.CoolantC)
	}
	if !approx(e.AccelMps2, -4.903325) || !approx(*e.TyreKpa, 206.84271) {
		t.Fatalf("accel/tyre conversion wrong: %v %v", e.AccelMps2, *e.TyreKpa)
	}
	if !e.Ignition || e.Charging {
		t.Fatal("bool_str mapping wrong")
	}
	if len(e.DTC) != 2 || e.DTC[0] != "P0217" || e.DTC[1] != "P0128" {
		t.Fatalf("dtc = %v", e.DTC)
	}
	if len(e.Events) != 2 {
		t.Fatalf("events = %v", e.Events)
	}
	if e.TsMs != 1790331302120 {
		t.Fatalf("ts = %d", e.TsMs)
	}
}

func TestStellar(t *testing.T) {
	a, _ := DefaultRegistry().Get("stellar")
	evs, rej, err := a.Decode(stellarBatch, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || len(rej) != 1 {
		t.Fatalf("events=%d rejects=%d", len(evs), len(rej))
	}
	e := evs[0]
	if !approx(e.SpeedKmh, 36) || !approx(e.OdoKm, 123.456) || !approx(*e.CoolantC, 90) {
		t.Fatalf("conversion wrong: %+v", e)
	}
	if e.TsMs != 1790000000500 || e.Seq != 42 || !e.Ignition || e.Charging {
		t.Fatalf("fields wrong: %+v", e)
	}
	if len(e.DTC) != 1 || e.DTC[0] != "P0A80" {
		t.Fatalf("dtc = %v", e.DTC)
	}
	if len(e.Events) != 1 || e.Events[0] != canonical.EvtHarshBrake {
		t.Fatalf("events = %v", e.Events)
	}
	if *e.SocPct != 55 || *e.PackTempC != 38 || *e.BattV != 12.4 || *e.RPM != 0 || *e.EngLoadPct != 10 || *e.FuelPct != 0 || *e.TyreKpa != 220 {
		t.Fatal("optional signals wrong")
	}
}

func TestDecode_Unparseable(t *testing.T) {
	r := DefaultRegistry()
	for _, oem := range []string{"aurora", "pinnacle", "stellar"} {
		a, _ := r.Get(oem)
		if _, _, err := a.Decode([]byte("{not json"), 0); err == nil {
			t.Errorf("%s: expected error", oem)
		}
		if _, _, err := a.Decode([]byte(`{"x":1}`), 0); err == nil {
			t.Errorf("%s: expected error for missing records", oem)
		}
	}
}

func TestStellar_MissingFields(t *testing.T) {
	a := NewStellarAdapter()
	_, rej, err := a.Decode([]byte(`{"messages":[{"t":1,"n":1},{"vin":"X","n":1},{"vin":"X","t":1}]}`), 0)
	if err != nil || len(rej) != 3 {
		t.Fatalf("err=%v rejects=%d", err, len(rej))
	}
}

func TestRegistry_UnknownAndList(t *testing.T) {
	r := DefaultRegistry()
	if _, err := r.Get("nope"); !errors.Is(err, ErrUnknownOEM) {
		t.Fatalf("got %v", err)
	}
	l := r.List()
	if len(l) != 3 || l[0] != "aurora@v1" {
		t.Fatalf("list = %v", l)
	}
}

func TestSpecValidation(t *testing.T) {
	bad := map[string]string{
		"not json":         `{`,
		"no oem":           `{"version":1,"fields":{}}`,
		"missing required": `{"oem":"x","version":1,"fields":{"vin":{"path":"v"}}}`,
		"optional required": `{"oem":"x","version":1,"fields":{"vin":{"path":"v","optional":true},"ts":{"path":"t"},"seq":{"path":"s"},
			"lat":{"path":"a"},"lon":{"path":"o"},"speed_kmh":{"path":"sp"},"odo_km":{"path":"od"}}}`,
		"unknown field": `{"oem":"x","version":1,"fields":{"vin":{"path":"v"},"ts":{"path":"t"},"seq":{"path":"s"},
			"lat":{"path":"a"},"lon":{"path":"o"},"speed_kmh":{"path":"sp"},"odo_km":{"path":"od"},"colour":{"path":"c"}}}`,
		"unknown unit": `{"oem":"x","version":1,"fields":{"vin":{"path":"v"},"ts":{"path":"t"},"seq":{"path":"s"},
			"lat":{"path":"a"},"lon":{"path":"o"},"speed_kmh":{"path":"sp","unit":"furlong"},"odo_km":{"path":"od"}}}`,
	}
	for name, s := range bad {
		if _, err := NewMappingAdapter([]byte(s)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// A new OEM is onboarded by dropping a spec file and reloading: no code change.
func TestHotOnboardNewOEM(t *testing.T) {
	dir := t.TempDir()
	spec := `{"oem":"nova","version":2,"records_path":"",
	 "fields":{"vin":{"path":"id"},"ts":{"path":"time","type":"epoch_s"},"seq":{"path":"n"},
	 "lat":{"path":"g.0"},"lon":{"path":"g.1"},"speed_kmh":{"path":"v","unit":"mps"},"odo_km":{"path":"d","unit":"m"},
	 "ignition":{"path":"on","type":"bool","optional":true},"soc_pct":{"path":"b","unit":"fraction","optional":true}}}`
	if err := os.WriteFile(filepath.Join(dir, "nova.json"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"oem":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := DefaultRegistry()
	loaded, errs := r.LoadSpecDir(dir)
	if len(loaded) != 1 || len(errs) != 1 {
		t.Fatalf("loaded=%v errs=%v", loaded, errs)
	}
	a, err := r.Get("nova")
	if err != nil {
		t.Fatal(err)
	}
	evs, rej, err := a.Decode([]byte(`[{"id":"1HGCM82633A004352","time":1790000000,"n":1,"g":[10.5,76.2],"v":20,"d":5000,"on":1,"b":0.8}]`), 0)
	if err != nil || len(rej) != 0 || len(evs) != 1 {
		t.Fatalf("err=%v rej=%v", err, rej)
	}
	e := evs[0]
	if !approx(e.SpeedKmh, 72) || !approx(e.OdoKm, 5) || !approx(*e.SocPct, 80) || !e.Ignition || e.Lat != 10.5 || e.Lon != 76.2 {
		t.Fatalf("decoded wrong: %+v", e)
	}
}

func TestMapping_TypeErrors(t *testing.T) {
	a, _ := DefaultRegistry().Get("aurora")
	cases := []string{
		`{"records":[{"timestamp":1,"sequence":1}]}`,                                                 // no vin
		`{"records":[{"vehicle":{"vin":"V"},"sequence":1}]}`,                                         // no ts
		`{"records":[{"vehicle":{"vin":"V"},"timestamp":"x","sequence":1}]}`,                         // bad ts
		`{"records":[{"vehicle":{"vin":"V"},"timestamp":1,"sequence":"a"}]}`,                         // bad seq
		`{"records":[{"vehicle":{"vin":"V"},"timestamp":1,"sequence":1,"position":{"latitude":1}}]}`, // no lon
		`{"records":[{"vehicle":{"vin":"V"},"timestamp":1,"sequence":1,"position":{"latitude":1,"longitude":1},"motion":{"speedKph":1},"powertrain":{"odometerKm":1,"engineOn":"yes"}}]}`,
	}
	for i, c := range cases {
		_, rej, err := a.Decode([]byte(c), 0)
		if err != nil || len(rej) != 1 {
			t.Errorf("case %d: err=%v rejects=%d", i, err, len(rej))
		}
	}
}

func BenchmarkAuroraDecode(b *testing.B) {
	a, _ := DefaultRegistry().Get("aurora")
	b.SetBytes(int64(len(auroraBatch)))
	for i := 0; i < b.N; i++ {
		_, _, _ = a.Decode(auroraBatch, 0)
	}
}
