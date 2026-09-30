package canonical

import (
	"errors"
	"testing"
)

const now = int64(1_790_000_000_000)

func valid() Event {
	return Event{
		EventID: ID("aurora", "1HGCM82633A004352", 1), V: SchemaVersion,
		VIN: "1HGCM82633A004352", OEM: "aurora", TsMs: now - 1000, Seq: 1,
		Lat: 12.97, Lon: 77.59, SpeedKmh: 40, OdoKm: 1000,
		CoolantC: F(90), BattV: F(12.6), FuelPct: F(50),
		DTC: []string{"P0301"}, Events: []string{EvtHarshBrake},
	}
}

func TestValidate_OK(t *testing.T) {
	e := valid()
	if err := e.Validate(now); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_Rejections(t *testing.T) {
	mut := map[string]func(*Event){
		"bad vin":     func(e *Event) { e.VIN = "BAD" },
		"no oem":      func(e *Event) { e.OEM = "" },
		"future":      func(e *Event) { e.TsMs = now + 10*60*1000 },
		"too old":     func(e *Event) { e.TsMs = now - 8*24*3600*1000 },
		"neg seq":     func(e *Event) { e.Seq = -1 },
		"lat":         func(e *Event) { e.Lat = 91 },
		"lon":         func(e *Event) { e.Lon = -181 },
		"speed":       func(e *Event) { e.SpeedKmh = 400 },
		"odo":         func(e *Event) { e.OdoKm = -1 },
		"soc":         func(e *Event) { e.SocPct = F(101) },
		"coolant":     func(e *Event) { e.CoolantC = F(500) },
		"battv":       func(e *Event) { e.BattV = F(99) },
		"many dtc":    func(e *Event) { e.DTC = make([]string, 40) },
		"unknown evt": func(e *Event) { e.Events = []string{"WHEELIE"} },
		"rpm":         func(e *Event) { e.RPM = F(20000) },
		"pack temp":   func(e *Event) { e.PackTempC = F(-100) },
		"engine load": func(e *Event) { e.EngLoadPct = F(120) },
		"fuel":        func(e *Event) { e.FuelPct = F(-1) },
	}
	for name, m := range mut {
		e := valid()
		m(&e)
		if err := e.Validate(now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}

func TestID_Deterministic(t *testing.T) {
	if ID("a", "V", 7) != ID("a", "V", 7) || ID("a", "V", 7) == ID("a", "V", 8) {
		t.Fatal("ID must be deterministic and seq-sensitive")
	}
}
