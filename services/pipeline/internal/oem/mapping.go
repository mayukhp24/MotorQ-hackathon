package oem

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/valyala/fastjson"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/dtc"
)

// FieldSpec maps one canonical field to a location in the OEM record.
type FieldSpec struct {
	Path       string            `json:"path"`
	Type       string            `json:"type,omitempty"` // number|string|bool|bool_str|epoch_ms|epoch_s|iso8601|csv|array
	Unit       string            `json:"unit,omitempty"`
	Optional   bool              `json:"optional,omitempty"`
	Map        map[string]string `json:"map,omitempty"`
	TrueValues []string          `json:"true_values,omitempty"`
	keys       []string
}

// Spec is a declarative OEM mapping (see contracts/oem-adapters/*.json).
type Spec struct {
	OEM         string               `json:"oem"`
	Version     int                  `json:"version"`
	RecordsPath string               `json:"records_path"`
	Fields      map[string]FieldSpec `json:"fields"`
}

var requiredFields = []string{"vin", "ts", "seq", "lat", "lon", "speed_kmh", "odo_km"}

var numericFields = map[string]bool{
	"seq": true, "lat": true, "lon": true, "heading_deg": true, "speed_kmh": true,
	"accel_mps2": true, "odo_km": true, "rpm": true, "coolant_c": true,
	"engine_load_pct": true, "fuel_pct": true, "soc_pct": true, "pack_temp_c": true,
	"batt_v": true, "tyre_min_kpa": true,
}

// optionalFloats lists nullable signals and where they live on the event.
var optionalFloats = []struct {
	name string
	dst  func(*canonical.Event) **float64
}{
	{"rpm", func(e *canonical.Event) **float64 { return &e.RPM }},
	{"coolant_c", func(e *canonical.Event) **float64 { return &e.CoolantC }},
	{"engine_load_pct", func(e *canonical.Event) **float64 { return &e.EngLoadPct }},
	{"fuel_pct", func(e *canonical.Event) **float64 { return &e.FuelPct }},
	{"soc_pct", func(e *canonical.Event) **float64 { return &e.SocPct }},
	{"pack_temp_c", func(e *canonical.Event) **float64 { return &e.PackTempC }},
	{"batt_v", func(e *canonical.Event) **float64 { return &e.BattV }},
	{"tyre_min_kpa", func(e *canonical.Event) **float64 { return &e.TyreKpa }},
}

var otherFields = map[string]bool{"vin": true, "ts": true, "ignition": true, "charging": true, "dtc": true, "evt": true}

// unitConv converts a source unit to the canonical unit of the field.
var unitConv = map[string]func(float64) float64{
	"":         func(x float64) float64 { return x },
	"kmh":      func(x float64) float64 { return x },
	"mph":      func(x float64) float64 { return x * 1.609344 },
	"mps":      func(x float64) float64 { return x * 3.6 },
	"km":       func(x float64) float64 { return x },
	"mi":       func(x float64) float64 { return x * 1.609344 },
	"m":        func(x float64) float64 { return x / 1000 },
	"degC":     func(x float64) float64 { return x },
	"degF":     func(x float64) float64 { return (x - 32) * 5 / 9 },
	"K":        func(x float64) float64 { return x - 273.15 },
	"kPa":      func(x float64) float64 { return x },
	"psi":      func(x float64) float64 { return x * 6.894757 },
	"bar":      func(x float64) float64 { return x * 100 },
	"pct":      func(x float64) float64 { return x },
	"fraction": func(x float64) float64 { return x * 100 },
	"V":        func(x float64) float64 { return x },
	"mV":       func(x float64) float64 { return x / 1000 },
	"g":        func(x float64) float64 { return x * 9.80665 },
	"mps2":     func(x float64) float64 { return x },
}

// MappingAdapter interprets a Spec at runtime. Parsing uses fastjson with a
// parser pool, so decoding a record allocates very little.
type MappingAdapter struct {
	spec        Spec
	recordsKeys []string
	pool        fastjson.ParserPool
}

func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, ".")
}

// NewMappingAdapter parses and validates a JSON spec (fail fast on load, not
// on the hot path).
func NewMappingAdapter(specJSON []byte) (*MappingAdapter, error) {
	var s Spec
	if err := json.Unmarshal(specJSON, &s); err != nil {
		return nil, fmt.Errorf("spec json: %w", err)
	}
	if s.OEM == "" || s.Version <= 0 {
		return nil, errors.New("spec: oem and positive version are required")
	}
	for _, f := range requiredFields {
		fs, ok := s.Fields[f]
		if !ok || fs.Path == "" {
			return nil, fmt.Errorf("spec %s: required field %q not mapped", s.OEM, f)
		}
		if fs.Optional {
			return nil, fmt.Errorf("spec %s: required field %q cannot be optional", s.OEM, f)
		}
	}
	for name, fs := range s.Fields {
		if !numericFields[name] && !otherFields[name] {
			return nil, fmt.Errorf("spec %s: unknown canonical field %q", s.OEM, name)
		}
		if _, ok := unitConv[fs.Unit]; !ok {
			return nil, fmt.Errorf("spec %s: field %q unknown unit %q", s.OEM, name, fs.Unit)
		}
		fs.keys = splitPath(fs.Path)
		s.Fields[name] = fs
	}
	return &MappingAdapter{spec: s, recordsKeys: splitPath(s.RecordsPath)}, nil
}

func (m *MappingAdapter) OEM() string  { return m.spec.OEM }
func (m *MappingAdapter) Version() int { return m.spec.Version }

func (m *MappingAdapter) Decode(payload []byte, recvMs int64) ([]canonical.Event, []Reject, error) {
	p := m.pool.Get()
	defer m.pool.Put(p)
	root, err := p.ParseBytes(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: unparseable payload: %w", m.spec.OEM, err)
	}
	recs := root
	if len(m.recordsKeys) > 0 {
		recs = root.Get(m.recordsKeys...)
	}
	if recs == nil {
		return nil, nil, fmt.Errorf("%s: records not found at %q", m.spec.OEM, m.spec.RecordsPath)
	}
	arr, err := recs.Array()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: records not found at %q", m.spec.OEM, m.spec.RecordsPath)
	}
	events := make([]canonical.Event, 0, len(arr))
	var rejects []Reject
	for _, r := range arr {
		ev, err := m.decodeRecord(r)
		if err != nil {
			rejects = append(rejects, Reject{OEM: m.spec.OEM, Reason: err.Error(), Raw: string(r.MarshalTo(nil))})
			continue
		}
		ev.IngestMs = recvMs
		events = append(events, ev)
	}
	return events, rejects, nil
}

func (m *MappingAdapter) num(r *fastjson.Value, field string) (*float64, error) {
	fs, ok := m.spec.Fields[field]
	if !ok {
		return nil, nil
	}
	v := r.Get(fs.keys...)
	if v == nil || v.Type() == fastjson.TypeNull {
		if fs.Optional {
			return nil, nil
		}
		return nil, fmt.Errorf("missing %s (%s)", field, fs.Path)
	}
	f, err := v.Float64()
	if err != nil {
		return nil, fmt.Errorf("%s: not a number", field)
	}
	f = unitConv[fs.Unit](f)
	return &f, nil
}

func (m *MappingAdapter) boolean(r *fastjson.Value, field string) (bool, error) {
	fs, ok := m.spec.Fields[field]
	if !ok {
		return false, nil
	}
	v := r.Get(fs.keys...)
	if v == nil || v.Type() == fastjson.TypeNull {
		if fs.Optional {
			return false, nil
		}
		return false, fmt.Errorf("missing %s", field)
	}
	switch fs.Type {
	case "bool_str":
		s := string(v.GetStringBytes())
		for _, t := range fs.TrueValues {
			if strings.EqualFold(s, t) {
				return true, nil
			}
		}
		return false, nil
	default:
		if v.Type() == fastjson.TypeNumber {
			return v.GetFloat64() != 0, nil
		}
		b, err := v.Bool()
		if err != nil {
			return false, fmt.Errorf("%s: not a bool", field)
		}
		return b, nil
	}
}

func (m *MappingAdapter) timestamp(r *fastjson.Value) (int64, error) {
	fs := m.spec.Fields["ts"]
	v := r.Get(fs.keys...)
	if v == nil {
		return 0, fmt.Errorf("missing ts (%s)", fs.Path)
	}
	switch fs.Type {
	case "iso8601":
		t, err := time.Parse(time.RFC3339Nano, string(v.GetStringBytes()))
		if err != nil {
			return 0, fmt.Errorf("ts: bad iso8601")
		}
		return t.UnixMilli(), nil
	case "epoch_s":
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("ts: not a number")
		}
		return int64(f * 1000), nil
	default: // epoch_ms
		i, err := v.Int64()
		if err != nil {
			f, ferr := v.Float64()
			if ferr != nil {
				return 0, fmt.Errorf("ts: not a number")
			}
			i = int64(f)
		}
		return i, nil
	}
}

// strList reads a list of codes either from a JSON array or a delimited string.
func (m *MappingAdapter) strList(r *fastjson.Value, field string) []string {
	fs, ok := m.spec.Fields[field]
	if !ok {
		return nil
	}
	v := r.Get(fs.keys...)
	if v == nil || v.Type() == fastjson.TypeNull {
		return nil
	}
	var raw []string
	if v.Type() == fastjson.TypeArray {
		for _, x := range v.GetArray() {
			raw = append(raw, string(x.GetStringBytes()))
		}
	} else {
		s := string(v.GetStringBytes())
		raw = strings.FieldsFunc(s, func(c rune) bool { return c == ',' || c == ';' || c == '|' || c == ' ' })
	}
	return raw
}

func (m *MappingAdapter) decodeRecord(r *fastjson.Value) (canonical.Event, error) {
	var e canonical.Event
	fs := m.spec.Fields["vin"]
	vv := r.Get(fs.keys...)
	if vv == nil {
		return e, fmt.Errorf("missing vin (%s)", fs.Path)
	}
	e.VIN = strings.ToUpper(strings.TrimSpace(string(vv.GetStringBytes())))
	e.OEM = m.spec.OEM
	e.V = canonical.SchemaVersion
	var err error
	if e.TsMs, err = m.timestamp(r); err != nil {
		return e, err
	}
	get := func(field string) (float64, error) {
		p, err := m.num(r, field)
		if err != nil || p == nil {
			return 0, err
		}
		return *p, nil
	}
	var seq float64
	if seq, err = get("seq"); err != nil {
		return e, err
	}
	e.Seq = int64(seq)
	if e.Lat, err = get("lat"); err != nil {
		return e, err
	}
	if e.Lon, err = get("lon"); err != nil {
		return e, err
	}
	if e.SpeedKmh, err = get("speed_kmh"); err != nil {
		return e, err
	}
	if e.OdoKm, err = get("odo_km"); err != nil {
		return e, err
	}
	if e.Heading, err = get("heading_deg"); err != nil {
		return e, err
	}
	if e.AccelMps2, err = get("accel_mps2"); err != nil {
		return e, err
	}
	for _, of := range optionalFloats {
		if *of.dst(&e), err = m.num(r, of.name); err != nil {
			return e, err
		}
	}
	if e.Ignition, err = m.boolean(r, "ignition"); err != nil {
		return e, err
	}
	if e.Charging, err = m.boolean(r, "charging"); err != nil {
		return e, err
	}
	for _, c := range m.strList(r, "dtc") {
		if code, ok := dtc.Normalize(c); ok {
			e.DTC = append(e.DTC, code)
		}
	}
	evMap := m.spec.Fields["evt"].Map
	for _, x := range m.strList(r, "evt") {
		if mapped, ok := evMap[x]; ok {
			x = mapped
		}
		if canonical.IsKnownEvent(x) {
			e.Events = append(e.Events, x)
		}
	}
	e.EventID = canonical.ID(e.OEM, e.VIN, e.Seq)
	return e, nil
}
