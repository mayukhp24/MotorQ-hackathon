// Package canonical defines the single normalised telemetry schema every OEM
// payload is mapped into (contracts/telemetry.v1.schema.json).
package canonical

import (
	"errors"
	"fmt"
	"strconv"

	"fleetpulse/pipeline/internal/vin"
)

const SchemaVersion = 1

// Event is one normalised vehicle signal sample. Optional signals are
// pointers so "not reported" (nil) is distinct from zero.
type Event struct {
	EventID    string   `json:"event_id"`
	V          int      `json:"v"`
	VIN        string   `json:"vin"`
	OEM        string   `json:"oem"`
	TsMs       int64    `json:"ts_ms"`
	Seq        int64    `json:"seq"`
	Lat        float64  `json:"lat"`
	Lon        float64  `json:"lon"`
	Heading    float64  `json:"heading_deg"`
	SpeedKmh   float64  `json:"speed_kmh"`
	AccelMps2  float64  `json:"accel_mps2"`
	OdoKm      float64  `json:"odo_km"`
	Ignition   bool     `json:"ignition"`
	Charging   bool     `json:"charging"`
	RPM        *float64 `json:"rpm,omitempty"`
	CoolantC   *float64 `json:"coolant_c,omitempty"`
	EngLoadPct *float64 `json:"engine_load_pct,omitempty"`
	FuelPct    *float64 `json:"fuel_pct,omitempty"`
	SocPct     *float64 `json:"soc_pct,omitempty"`
	PackTempC  *float64 `json:"pack_temp_c,omitempty"`
	BattV      *float64 `json:"batt_v,omitempty"`
	TyreKpa    *float64 `json:"tyre_min_kpa,omitempty"`
	DTC        []string `json:"dtc,omitempty"`
	Events     []string `json:"evt,omitempty"`
	IngestMs   int64    `json:"ingest_ms"`
}

// Enriched is what the stream processor publishes for analytics sinks.
type Enriched struct {
	Event
	TenantID   string `json:"tenant_id"`
	FleetID    string `json:"fleet_id"`
	Geohash    string `json:"geohash"`
	Powertrain string `json:"powertrain"`
	ProcMs     int64  `json:"proc_ms"`
}

// Known discrete events.
const (
	EvtHarshBrake = "HARSH_BRAKE"
	EvtHarshAccel = "HARSH_ACCEL"
	EvtHarshTurn  = "HARSH_CORNER"
	EvtCrash      = "CRASH"
)

var knownEvents = map[string]bool{EvtHarshBrake: true, EvtHarshAccel: true, EvtHarshTurn: true, EvtCrash: true}

// IsKnownEvent reports whether e is part of the canonical event vocabulary.
func IsKnownEvent(e string) bool { return knownEvents[e] }

// ID returns the deterministic idempotency key for an event. The same
// physical sample re-sent by an OEM (retry, replay) gets the same ID.
func ID(oem, v string, seq int64) string {
	return oem + ":" + v + ":" + strconv.FormatInt(seq, 10)
}

// Validation limits. Values outside physical ranges are rejected (fail fast)
// rather than silently clamped.
const (
	maxClockSkewMs = 5 * 60 * 1000
	maxAgeMs       = 7 * 24 * 3600 * 1000
	maxDTCs        = 32
)

var ErrInvalid = errors.New("invalid event")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func inRange(p *float64, lo, hi float64) bool { return p == nil || (*p >= lo && *p <= hi) }

// Validate enforces the telemetry.v1 contract. nowMs is injected for
// deterministic tests.
func (e *Event) Validate(nowMs int64) error {
	if err := vin.Validate(e.VIN); err != nil {
		return invalid("vin %q: %v", e.VIN, err)
	}
	if e.OEM == "" {
		return invalid("missing oem")
	}
	if e.TsMs > nowMs+maxClockSkewMs {
		return invalid("timestamp %d in the future", e.TsMs)
	}
	if e.TsMs < nowMs-maxAgeMs {
		return invalid("timestamp %d older than retention window", e.TsMs)
	}
	if e.Seq < 0 {
		return invalid("negative seq")
	}
	if e.Lat < -90 || e.Lat > 90 || e.Lon < -180 || e.Lon > 180 {
		return invalid("coordinates out of range")
	}
	if e.SpeedKmh < 0 || e.SpeedKmh > 300 {
		return invalid("speed %.1f out of range", e.SpeedKmh)
	}
	if e.OdoKm < 0 {
		return invalid("negative odometer")
	}
	if !inRange(e.SocPct, 0, 100) || !inRange(e.FuelPct, 0, 100) {
		return invalid("soc/fuel out of range")
	}
	if !inRange(e.CoolantC, -50, 180) || !inRange(e.PackTempC, -50, 120) {
		return invalid("temperature out of range")
	}
	if !inRange(e.BattV, 0, 30) || !inRange(e.RPM, 0, 12000) || !inRange(e.EngLoadPct, 0, 100) {
		return invalid("electrical/engine signal out of range")
	}
	if len(e.DTC) > maxDTCs {
		return invalid("too many DTCs")
	}
	for _, ev := range e.Events {
		if !IsKnownEvent(ev) {
			return invalid("unknown event %q", ev)
		}
	}
	return nil
}

// F is a helper for building optional float fields.
func F(v float64) *float64 { return &v }
