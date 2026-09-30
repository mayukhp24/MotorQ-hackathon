// Package detect turns a per-vehicle event stream into alerts and trip
// summaries. It is pure domain logic: no I/O, no clocks, so every rule is
// unit-testable with synthetic sequences.
package detect

import (
	"fmt"
	"math"

	"github.com/google/uuid"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/dtc"
)

// Severity levels, ordered.
const (
	SevCritical = "CRITICAL"
	SevHigh     = "HIGH"
	SevMedium   = "MEDIUM"
	SevLow      = "LOW"
)

// Alert types.
const (
	AlertCriticalDTC   = "CRITICAL_DTC"
	AlertMajorDTC      = "MAJOR_DTC"
	AlertOverheat      = "ENGINE_OVERHEAT"
	AlertCoolantHigh   = "COOLANT_HIGH"
	AlertLowVoltage    = "LOW_BATTERY_VOLTAGE"
	AlertPackOverTemp  = "EV_PACK_OVERTEMP"
	AlertLowSoC        = "LOW_STATE_OF_CHARGE"
	AlertCrash         = "CRASH_DETECTED"
	AlertRiskyDriving  = "RISKY_DRIVING"
	AlertExcessiveIdle = "EXCESSIVE_IDLE"
	AlertOverspeed     = "OVERSPEED"
	AlertTyrePressure  = "TYRE_PRESSURE_LOW"
)

// Thresholds are tunable per deployment (12-factor: overridable via env).
type Thresholds struct {
	OverheatC         float64
	OverheatHoldMs    int64
	CoolantHighC      float64
	CoolantHighHoldMs int64
	LowVoltV          float64
	LowVoltHoldMs     int64
	PackOverTempC     float64
	LowSoCPct         float64
	HarshDecelMps2    float64
	HarshAccelMps2    float64
	RiskyWindowMs     int64
	RiskyCount        int
	IdleMs            int64
	OverspeedKmh      float64
	OverspeedHoldMs   int64
	TyreLowKpa        float64
	CooldownMs        int64
	MinTripKm         float64
	IdleFuelLph       float64 // litres/hour burnt idling (ICE)
	FuelPricePerL     float64
	EVIdleKw          float64 // HVAC load while idling (EV)
	PowerPricePerKwh  float64
	MaxGapForRateMs   int64 // max gap between samples to derive accel
	LateToleranceMs   int64
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		OverheatC: 110, OverheatHoldMs: 15_000,
		CoolantHighC: 104, CoolantHighHoldMs: 60_000,
		LowVoltV: 11.8, LowVoltHoldMs: 30_000,
		PackOverTempC: 55, LowSoCPct: 10,
		HarshDecelMps2: -6.5, HarshAccelMps2: 4.5,
		RiskyWindowMs: 5 * 60_000, RiskyCount: 3,
		IdleMs:       10 * 60_000,
		OverspeedKmh: 120, OverspeedHoldMs: 30_000,
		TyreLowKpa:  180,
		CooldownMs:  15 * 60_000,
		MinTripKm:   0.2,
		IdleFuelLph: 0.9, FuelPricePerL: 1.15,
		EVIdleKw: 2.0, PowerPricePerKwh: 0.18,
		MaxGapForRateMs: 3_000,
		LateToleranceMs: 0,
	}
}

// VehicleMeta is static reference data joined onto the stream.
type VehicleMeta struct {
	TenantID   string
	FleetID    string
	DriverID   string
	Powertrain string // ICE | EV | HEV
}

// Alert is published on alerts.v1 (contracts/alert.v1.schema.json).
type Alert struct {
	AlertID    string         `json:"alert_id"`
	TenantID   string         `json:"tenant_id"`
	FleetID    string         `json:"fleet_id"`
	VIN        string         `json:"vin"`
	Type       string         `json:"type"`
	Severity   string         `json:"severity"`
	Title      string         `json:"title"`
	TsMs       int64          `json:"ts_ms"`
	IngestMs   int64          `json:"ingest_ms"`
	DetectedMs int64          `json:"detected_ms"`
	Lat        float64        `json:"lat"`
	Lon        float64        `json:"lon"`
	Details    map[string]any `json:"details"`
}

// Trip is published on trips.v1 when a trip closes.
type Trip struct {
	TripID       string  `json:"trip_id"`
	TenantID     string  `json:"tenant_id"`
	FleetID      string  `json:"fleet_id"`
	VIN          string  `json:"vin"`
	DriverID     string  `json:"driver_id,omitempty"`
	StartMs      int64   `json:"start_ms"`
	EndMs        int64   `json:"end_ms"`
	StartLat     float64 `json:"start_lat"`
	StartLon     float64 `json:"start_lon"`
	EndLat       float64 `json:"end_lat"`
	EndLon       float64 `json:"end_lon"`
	DistanceKm   float64 `json:"distance_km"`
	MaxSpeedKmh  float64 `json:"max_speed_kmh"`
	IdleSec      int64   `json:"idle_s"`
	HarshBrakes  int     `json:"harsh_brakes"`
	HarshAccels  int     `json:"harsh_accels"`
	OverspeedSec int64   `json:"overspeed_s"`
	EnergyUsed   float64 `json:"energy_used_pct"`
}

// Status of a vehicle for the live dashboard.
const (
	StatusDriving  = "DRIVING"
	StatusIdling   = "IDLING"
	StatusCharging = "CHARGING"
	StatusParked   = "PARKED"
)

// State is the per-vehicle working memory (bounded: fixed fields plus a
// tiny ring of recent harsh-event timestamps).
type State struct {
	LastTs, LastSeq int64
	LastSpeed       float64
	Seen            bool
	Status          string
	coolHighSince   int64
	overheatSince   int64
	lowVoltSince    int64
	idleSince       int64
	overspeedSince  int64
	harsh           [8]int64
	harshN          int
	lastAlert       map[string]int64
	activeDTC       map[string]struct{}
	trip            *tripAcc
	LateEvents      int64
}

type tripAcc struct {
	startMs                int64
	startLat, startLon     float64
	startOdo, lastOdo      float64
	lastLat, lastLon       float64
	maxSpeed               float64
	idleMs, overspeedMs    int64
	harshBrakes, harshAcc  int
	energyStart, energyEnd float64
	lastTs                 int64
}

func NewState() *State {
	return &State{lastAlert: map[string]int64{}, activeDTC: map[string]struct{}{}, Status: StatusParked}
}

// Engine evaluates rules. It is stateless apart from configuration.
type Engine struct {
	T Thresholds
}

func NewEngine(t Thresholds) *Engine { return &Engine{T: t} }

var alertNS = uuid.MustParse("6f1c1f0e-8d4b-4c38-9e59-5b1f4b8b2a10")

// alertID is deterministic per (vin, type, cooldown bucket) so that replays
// and duplicates produce the same ID (idempotent sinks).
func alertID(vin, typ string, bucket int64) string {
	return uuid.NewSHA1(alertNS, []byte(fmt.Sprintf("%s|%s|%d", vin, typ, bucket))).String()
}

func tripID(vin string, startMs int64) string {
	return uuid.NewSHA1(alertNS, []byte(fmt.Sprintf("trip|%s|%d", vin, startMs))).String()
}

func energy(e *canonical.Event) float64 {
	if e.SocPct != nil {
		return *e.SocPct
	}
	if e.FuelPct != nil {
		return *e.FuelPct
	}
	return math.NaN()
}

// Result of processing one event.
type Result struct {
	Alerts []Alert
	Trip   *Trip
	Late   bool
}

// Process applies one event to the vehicle state. Late (out-of-order) events
// are flagged and must not mutate live state; they are still persisted by
// the analytics sink.
func (g *Engine) Process(st *State, e *canonical.Event, meta VehicleMeta, nowMs int64) Result {
	var res Result
	if st.Seen && e.TsMs <= st.LastTs-g.T.LateToleranceMs {
		st.LateEvents++
		res.Late = true
		return res
	}
	// emitKey applies a per-(vehicle, key) cooldown so a persisting condition
	// raises one alert, not one per second.
	emitKey := func(key, typ, sev, title string, details map[string]any) {
		bucket := e.TsMs / g.T.CooldownMs
		if last, ok := st.lastAlert[key]; ok && e.TsMs-last < g.T.CooldownMs {
			return
		}
		st.lastAlert[key] = e.TsMs
		res.Alerts = append(res.Alerts, Alert{
			AlertID: alertID(e.VIN, key, bucket), TenantID: meta.TenantID, FleetID: meta.FleetID,
			VIN: e.VIN, Type: typ, Severity: sev, Title: title, TsMs: e.TsMs,
			IngestMs: e.IngestMs, DetectedMs: nowMs, Lat: e.Lat, Lon: e.Lon, Details: details,
		})
	}
	emit := func(typ, sev, title string, details map[string]any) { emitKey(typ, typ, sev, title, details) }

	dt := int64(0)
	if st.Seen {
		dt = e.TsMs - st.LastTs
	}

	// --- Diagnostics: alert on newly appearing codes only.
	if len(e.DTC) == 0 {
		if len(st.activeDTC) > 0 {
			st.activeDTC = map[string]struct{}{}
		}
	} else {
		current := make(map[string]struct{}, len(e.DTC))
		for _, c := range e.DTC {
			current[c] = struct{}{}
			if _, active := st.activeDTC[c]; active {
				continue
			}
			info := dtc.Lookup(c)
			details := map[string]any{"dtc": c, "component": string(info.Component)}
			switch info.Severity {
			case dtc.Critical:
				emitKey(AlertCriticalDTC+":"+c, AlertCriticalDTC, SevCritical, info.Description, details)
			case dtc.Major:
				emitKey(AlertMajorDTC+":"+c, AlertMajorDTC, SevMedium, info.Description, details)
			}
		}
		st.activeDTC = current
	}

	// --- Engine temperature (sustained-threshold rules avoid sensor spikes).
	if e.CoolantC != nil {
		c := *e.CoolantC
		st.overheatSince = holdSince(st.overheatSince, c >= g.T.OverheatC, e.TsMs)
		st.coolHighSince = holdSince(st.coolHighSince, c >= g.T.CoolantHighC, e.TsMs)
		if st.overheatSince > 0 && e.TsMs-st.overheatSince >= g.T.OverheatHoldMs {
			emit(AlertOverheat, SevCritical, "Engine overheating – stop vehicle safely",
				map[string]any{"coolant_c": round1(c), "threshold_c": g.T.OverheatC})
		} else if st.coolHighSince > 0 && e.TsMs-st.coolHighSince >= g.T.CoolantHighHoldMs {
			emit(AlertCoolantHigh, SevHigh, "Coolant temperature trending high",
				map[string]any{"coolant_c": round1(c), "threshold_c": g.T.CoolantHighC})
		}
	}

	// --- 12 V system voltage (only meaningful with ignition on).
	if e.BattV != nil {
		st.lowVoltSince = holdSince(st.lowVoltSince, e.Ignition && *e.BattV < g.T.LowVoltV, e.TsMs)
		if st.lowVoltSince > 0 && e.TsMs-st.lowVoltSince >= g.T.LowVoltHoldMs {
			emit(AlertLowVoltage, SevHigh, "12 V battery/charging system voltage low",
				map[string]any{"batt_v": round1(*e.BattV), "threshold_v": g.T.LowVoltV})
		}
	}

	// --- EV pack.
	if e.PackTempC != nil && *e.PackTempC >= g.T.PackOverTempC {
		emit(AlertPackOverTemp, SevCritical, "EV battery pack over-temperature",
			map[string]any{"pack_temp_c": round1(*e.PackTempC), "threshold_c": g.T.PackOverTempC})
	}
	if e.SocPct != nil && *e.SocPct < g.T.LowSoCPct && !e.Charging && e.Ignition {
		emit(AlertLowSoC, SevMedium, "State of charge critically low",
			map[string]any{"soc_pct": round1(*e.SocPct)})
	}

	if e.TyreKpa != nil && *e.TyreKpa < g.T.TyreLowKpa {
		emit(AlertTyrePressure, SevMedium, "Tyre pressure low",
			map[string]any{"tyre_min_kpa": round1(*e.TyreKpa), "threshold_kpa": g.T.TyreLowKpa})
	}

	// --- Driving behaviour: OEM-flagged events plus derived acceleration.
	accel := e.AccelMps2
	if accel == 0 && st.Seen && dt > 0 && dt <= g.T.MaxGapForRateMs {
		accel = (e.SpeedKmh - st.LastSpeed) / 3.6 / (float64(dt) / 1000)
	}
	harshBrake, harshAccel := accel <= g.T.HarshDecelMps2, accel >= g.T.HarshAccelMps2
	for _, ev := range e.Events {
		switch ev {
		case canonical.EvtCrash:
			emit(AlertCrash, SevCritical, "Possible collision detected",
				map[string]any{"speed_kmh": round1(st.LastSpeed)})
		case canonical.EvtHarshBrake:
			harshBrake = true
		case canonical.EvtHarshAccel:
			harshAccel = true
		}
	}
	if harshBrake || harshAccel {
		st.harsh[st.harshN%len(st.harsh)] = e.TsMs
		st.harshN++
		if n := countSince(st.harsh[:], e.TsMs-g.T.RiskyWindowMs); n >= g.T.RiskyCount {
			emit(AlertRiskyDriving, SevMedium, "Repeated harsh driving events",
				map[string]any{"events_in_window": n, "window_min": g.T.RiskyWindowMs / 60_000})
		}
	}

	st.overspeedSince = holdSince(st.overspeedSince, e.SpeedKmh > g.T.OverspeedKmh, e.TsMs)
	if st.overspeedSince > 0 && e.TsMs-st.overspeedSince >= g.T.OverspeedHoldMs {
		emit(AlertOverspeed, SevMedium, "Sustained overspeed",
			map[string]any{"speed_kmh": round1(e.SpeedKmh), "limit_kmh": g.T.OverspeedKmh})
	}

	// --- Idling cost: ignition on, stationary, not charging.
	idling := e.Ignition && e.SpeedKmh < 1 && !e.Charging
	st.idleSince = holdSince(st.idleSince, idling, e.TsMs)
	if st.idleSince > 0 && e.TsMs-st.idleSince >= g.T.IdleMs {
		mins := float64(e.TsMs-st.idleSince) / 60_000
		cost := mins / 60 * g.T.IdleFuelLph * g.T.FuelPricePerL
		if meta.Powertrain == "EV" {
			cost = mins / 60 * g.T.EVIdleKw * g.T.PowerPricePerKwh
		}
		emit(AlertExcessiveIdle, SevLow, "Excessive idling",
			map[string]any{"idle_min": round1(mins), "est_cost_usd": math.Round(cost*100) / 100})
	}

	// --- Status + trip segmentation (state machine PARKED -> DRIVING/IDLING -> PARKED).
	switch {
	case e.Charging:
		st.Status = StatusCharging
	case e.Ignition && e.SpeedKmh >= 3:
		st.Status = StatusDriving
	case e.Ignition:
		st.Status = StatusIdling
	default:
		st.Status = StatusParked
	}
	res.Trip = g.trip(st, e, meta, dt, harshBrake, harshAccel)

	st.LastTs, st.LastSeq, st.LastSpeed, st.Seen = e.TsMs, e.Seq, e.SpeedKmh, true
	return res
}

func (g *Engine) trip(st *State, e *canonical.Event, meta VehicleMeta, dt int64, hb, ha bool) *Trip {
	if st.trip == nil {
		if e.Ignition && e.SpeedKmh >= 5 {
			st.trip = &tripAcc{startMs: e.TsMs, startLat: e.Lat, startLon: e.Lon, startOdo: e.OdoKm,
				lastOdo: e.OdoKm, lastLat: e.Lat, lastLon: e.Lon, energyStart: energy(e), energyEnd: energy(e), lastTs: e.TsMs}
		}
		return nil
	}
	t := st.trip
	if e.Ignition {
		if dt > 0 && dt <= 60_000 {
			if e.SpeedKmh < 1 {
				t.idleMs += dt
			}
			if e.SpeedKmh > g.T.OverspeedKmh {
				t.overspeedMs += dt
			}
		}
		t.maxSpeed = math.Max(t.maxSpeed, e.SpeedKmh)
		if hb {
			t.harshBrakes++
		}
		if ha {
			t.harshAcc++
		}
		t.lastOdo, t.lastLat, t.lastLon, t.lastTs = e.OdoKm, e.Lat, e.Lon, e.TsMs
		if en := energy(e); !math.IsNaN(en) {
			t.energyEnd = en
		}
		return nil
	}
	// Ignition off closes the trip.
	st.trip = nil
	dist := t.lastOdo - t.startOdo
	if dist < g.T.MinTripKm {
		return nil
	}
	used := t.energyStart - t.energyEnd
	if math.IsNaN(used) {
		used = 0
	}
	return &Trip{
		TripID: tripID(e.VIN, t.startMs), TenantID: meta.TenantID, FleetID: meta.FleetID, VIN: e.VIN,
		DriverID: meta.DriverID, StartMs: t.startMs, EndMs: t.lastTs,
		StartLat: t.startLat, StartLon: t.startLon, EndLat: t.lastLat, EndLon: t.lastLon,
		DistanceKm: round1(dist), MaxSpeedKmh: round1(t.maxSpeed), IdleSec: t.idleMs / 1000,
		HarshBrakes: t.harshBrakes, HarshAccels: t.harshAcc, OverspeedSec: t.overspeedMs / 1000,
		EnergyUsed: round1(used),
	}
}

// holdSince tracks when a condition started being continuously true.
func holdSince(since int64, cond bool, ts int64) int64 {
	if !cond {
		return 0
	}
	if since == 0 {
		return ts
	}
	return since
}

func countSince(ring []int64, from int64) int {
	n := 0
	for _, ts := range ring {
		if ts > 0 && ts >= from {
			n++
		}
	}
	return n
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
