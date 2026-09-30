package oem

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/valyala/fastjson"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/dtc"
)

//go:embed specs/aurora.json
var AuroraSpec []byte

//go:embed specs/pinnacle.json
var PinnacleSpec []byte

// StellarAdapter handles a signal-list format where every reading is a
// {name, value, unit} tuple. Structure varies per message, so a declarative
// path mapping does not fit; this is a hand-written Strategy.
type StellarAdapter struct {
	pool fastjson.ParserPool
}

func NewStellarAdapter() *StellarAdapter { return &StellarAdapter{} }

func (s *StellarAdapter) OEM() string  { return "stellar" }
func (s *StellarAdapter) Version() int { return 1 }

var stellarAlerts = map[string]string{
	"harshBraking":      canonical.EvtHarshBrake,
	"harshAcceleration": canonical.EvtHarshAccel,
	"harshCornering":    canonical.EvtHarshTurn,
	"collision":         canonical.EvtCrash,
}

func (s *StellarAdapter) Decode(payload []byte, recvMs int64) ([]canonical.Event, []Reject, error) {
	p := s.pool.Get()
	defer s.pool.Put(p)
	root, err := p.ParseBytes(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("stellar: unparseable payload: %w", err)
	}
	msgs := root.GetArray("messages")
	if msgs == nil {
		return nil, nil, fmt.Errorf("stellar: messages[] not found")
	}
	events := make([]canonical.Event, 0, len(msgs))
	var rejects []Reject
	for _, m := range msgs {
		e, err := s.decode(m)
		if err != nil {
			rejects = append(rejects, Reject{OEM: "stellar", Reason: err.Error(), Raw: string(m.MarshalTo(nil))})
			continue
		}
		e.IngestMs = recvMs
		events = append(events, e)
	}
	return events, rejects, nil
}

func (s *StellarAdapter) decode(m *fastjson.Value) (canonical.Event, error) {
	var e canonical.Event
	e.V = canonical.SchemaVersion
	e.OEM = "stellar"
	e.VIN = strings.ToUpper(string(m.GetStringBytes("vin")))
	if e.VIN == "" {
		return e, fmt.Errorf("missing vin")
	}
	t := m.Get("t")
	if t == nil {
		return e, fmt.Errorf("missing t")
	}
	e.TsMs = int64(t.GetFloat64() * 1000)
	n := m.Get("n")
	if n == nil {
		return e, fmt.Errorf("missing n")
	}
	e.Seq = n.GetInt64()
	var haveLat, haveLon, haveSpeed, haveOdo bool
	for _, sig := range m.GetArray("signals") {
		name := string(sig.GetStringBytes("name"))
		unit := string(sig.GetStringBytes("unit"))
		v := sig.GetFloat64("value")
		switch name {
		case "gps.lat":
			e.Lat, haveLat = v, true
		case "gps.lon":
			e.Lon, haveLon = v, true
		case "gps.heading":
			e.Heading = v
		case "speed":
			if unit == "m/s" {
				v *= 3.6
			}
			e.SpeedKmh, haveSpeed = v, true
		case "accel.long":
			e.AccelMps2 = v
		case "odometer":
			if unit == "m" {
				v /= 1000
			}
			e.OdoKm, haveOdo = v, true
		case "ignition":
			e.Ignition = v != 0
		case "rpm":
			e.RPM = canonical.F(v)
		case "coolant.temp":
			if unit == "K" {
				v -= 273.15
			}
			e.CoolantC = canonical.F(v)
		case "engine.load":
			e.EngLoadPct = canonical.F(v)
		case "fuel.level":
			e.FuelPct = canonical.F(v)
		case "hv.soc":
			e.SocPct = canonical.F(v)
		case "hv.temp":
			e.PackTempC = canonical.F(v)
		case "hv.charging":
			e.Charging = v != 0
		case "lv.voltage":
			e.BattV = canonical.F(v)
		case "tpms.min":
			e.TyreKpa = canonical.F(v)
		}
	}
	if !haveLat || !haveLon || !haveSpeed || !haveOdo {
		return e, fmt.Errorf("missing mandatory signal (gps/speed/odometer)")
	}
	for _, f := range m.GetArray("faults") {
		if code, ok := dtc.FromParts(string(f.GetStringBytes("type")), string(f.GetStringBytes("code"))); ok {
			e.DTC = append(e.DTC, code)
		}
	}
	for _, a := range m.GetArray("alerts") {
		if ev, ok := stellarAlerts[string(a.GetStringBytes())]; ok {
			e.Events = append(e.Events, ev)
		}
	}
	e.EventID = canonical.ID(e.OEM, e.VIN, e.Seq)
	return e, nil
}
