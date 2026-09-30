package sim

import (
	"math"
	"math/rand/v2"
	"time"
)

// Components with a degradation process. These are the failure modes the
// predictive-maintenance model learns to anticipate.
const (
	CompCooling = "cooling"
	CompBattery = "battery12v"
	CompMisfire = "misfire"
	CompEVPack  = "ev_pack"
)

// Components lists all modelled components.
var Components = []string{CompCooling, CompBattery, CompMisfire, CompEVPack}

// Daily onset hazard per component (probability a degradation episode starts
// on a given day). Tuned so roughly 10-15% of vehicles break down per 60 days.
var hazard = map[string]float64{
	CompCooling: 0.0008,
	CompBattery: 0.0009,
	CompMisfire: 0.0006,
	CompEVPack:  0.0007,
}

// BreakdownCostUSD: unplanned (tow + repair + downtime) vs planned repair.
var BreakdownCostUSD = map[string]float64{CompCooling: 2400, CompBattery: 650, CompMisfire: 1900, CompEVPack: 7800}
var PlannedCostUSD = map[string]float64{CompCooling: 520, CompBattery: 180, CompMisfire: 450, CompEVPack: 2600}

// Episode is one degradation → failure → repair cycle. Days are absolute
// (days since Unix epoch, fractional).
type Episode struct {
	Component  string
	Onset      float64
	Failure    float64
	RepairDays float64
	Silent     bool // fails without precursors (irreducible error)
}

// Timeline window for episode generation, fixed so results do not depend on
// when the generator runs.
var (
	timelineStart = float64(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()) / 86400
	timelineEnd   = float64(time.Date(2028, 12, 31, 0, 0, 0, 0, time.UTC).Unix()) / 86400
)

func applies(comp, powertrain string) (bool, float64) {
	switch comp {
	case CompCooling, CompMisfire:
		return powertrain != "EV", 1
	case CompEVPack:
		if powertrain == "EV" {
			return true, 1
		}
		return powertrain == "HEV", 0.3
	default:
		return true, 1
	}
}

// GenerateEpisodes draws a renewal process per component: exponential
// time-to-onset, 6-20 day degradation, 1-3 day repair.
func GenerateEpisodes(v *Vehicle) []Episode {
	r := rand.New(rand.NewPCG(v.Seed, 0xE915))
	var eps []Episode
	// Older, high-mileage vehicles degrade faster.
	ageFactor := 1 + 0.08*float64(2026-v.Year)
	for _, c := range Components {
		ok, scale := applies(c, v.Powertrain)
		if !ok {
			continue
		}
		h := hazard[c] * scale * ageFactor
		t := timelineStart + r.ExpFloat64()/h
		for t < timelineEnd {
			dur := 6 + r.Float64()*14
			e := Episode{Component: c, Onset: t, Failure: t + dur, RepairDays: 1 + r.Float64()*2, Silent: r.Float64() < 0.2}
			eps = append(eps, e)
			t = e.Failure + e.RepairDays + r.ExpFloat64()/h
		}
	}
	return eps
}

// Health describes a vehicle's hidden condition at an instant.
type Health struct {
	Sev      map[string]float64 // 0 healthy .. 1 failing (visible precursors only)
	InRepair bool
}

// HealthAt evaluates the hidden state at absolute day d.
func (v *Vehicle) HealthAt(d float64) Health {
	h := Health{Sev: map[string]float64{}}
	for _, e := range v.Episodes {
		if d >= e.Failure && d < e.Failure+e.RepairDays {
			h.InRepair = true
		}
		if d >= e.Onset && d < e.Failure && !e.Silent {
			s := (d - e.Onset) / (e.Failure - e.Onset)
			if s > h.Sev[e.Component] {
				h.Sev[e.Component] = s
			}
		}
	}
	return h
}

// FailuresBetween returns episodes whose failure day lies in [from, to).
func (v *Vehicle) FailuresBetween(from, to float64) []Episode {
	var out []Episode
	for _, e := range v.Episodes {
		if e.Failure >= from && e.Failure < to {
			out = append(out, e)
		}
	}
	return out
}

// Ambient returns the ambient temperature (°C) for a city on an absolute day.
func Ambient(c City, day float64, r *rand.Rand) float64 {
	season := math.Sin(2 * math.Pi * (day - 100) / 365.25)
	return c.TempMean + c.TempAmp*season + r.NormFloat64()*2
}

// Signal offsets per unit of degradation, shared by the daily back-fill and
// the 1 Hz stream so both show the same physics.
func coolantOffset(s float64) float64  { return 17 * math.Pow(s, 1.6) }
func voltageDrop(s float64) float64    { return 1.7 * math.Pow(s, 1.5) }
func packTempOffset(s float64) float64 { return 14 * math.Pow(s, 1.5) }

// DTC occurrence rates (per active hour) as a function of degradation.
// Codes are reported as discrete fault events, not repeated every sample.
type dtcRate struct {
	Code string
	Rate func(sev map[string]float64, pt string) float64
}

var dtcRates = []dtcRate{
	{"P0128", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.004+0.12*s[CompCooling]) }},
	{"P0217", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.35*math.Pow(s[CompCooling], 3)) }},
	{"P0480", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.05*s[CompCooling]*s[CompCooling]) }},
	{"P0562", func(s map[string]float64, pt string) float64 { return 0.002 + 0.25*math.Pow(s[CompBattery], 2.2) }},
	{"P0620", func(s map[string]float64, pt string) float64 { return 0.04 * math.Pow(s[CompBattery], 2) }},
	{"P0300", func(s map[string]float64, pt string) float64 {
		return iceOnly(pt, 0.002+0.2*math.Pow(s[CompMisfire], 2))
	}},
	{"P0301", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.15*math.Pow(s[CompMisfire], 1.8)) }},
	{"P0302", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.08*math.Pow(s[CompMisfire], 2)) }},
	{"P0A7F", func(s map[string]float64, pt string) float64 {
		return evOnly(pt, 0.001+0.18*math.Pow(s[CompEVPack], 2))
	}},
	{"P0AFA", func(s map[string]float64, pt string) float64 { return evOnly(pt, 0.08*s[CompEVPack]) }},
	{"P0A80", func(s map[string]float64, pt string) float64 { return evOnly(pt, 0.3*math.Pow(s[CompEVPack], 4)) }},
	// Benign background noise every fleet sees: makes "any DTC" a poor predictor.
	{"P0442", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.006) }},
	{"P0456", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.004) }},
	{"P0171", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.004) }},
	{"P0420", func(s map[string]float64, pt string) float64 { return iceOnly(pt, 0.003) }},
	{"U0100", func(s map[string]float64, pt string) float64 { return 0.004 }},
	{"C0750", func(s map[string]float64, pt string) float64 { return 0.003 }},
}

func iceOnly(pt string, x float64) float64 {
	if pt == "EV" {
		return 0
	}
	return x
}

func evOnly(pt string, x float64) float64 {
	if pt == "ICE" {
		return 0
	}
	return x
}

// DTCComponent maps codes to the ML feature buckets.
var DTCComponent = map[string]string{
	"P0128": CompCooling, "P0217": CompCooling, "P0480": CompCooling, "P0115": CompCooling, "P0116": CompCooling,
	"P0562": CompBattery, "P0620": CompBattery, "P0563": CompBattery, "P0A0F": CompBattery,
	"P0300": CompMisfire, "P0301": CompMisfire, "P0302": CompMisfire, "P0303": CompMisfire, "P0304": CompMisfire,
	"P0A7F": CompEVPack, "P0AFA": CompEVPack, "P0A80": CompEVPack, "P0AA6": CompEVPack, "P0C73": CompEVPack,
}

func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x5EED)) }
