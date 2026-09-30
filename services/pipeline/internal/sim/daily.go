package sim

import (
	"math"
	"math/rand/v2"
	"time"
)

// DailyRow mirrors the ClickHouse vehicle_daily table (one row per vehicle
// per active day). Sample counts are 1 Hz equivalents.
type DailyRow struct {
	TenantID      string
	VIN           string
	Day           time.Time
	Samples       uint32
	IgnSamples    uint32
	IdleSamples   uint32
	MovingSamples uint32
	OdoMin        float64
	OdoMax        float64
	MaxSpeed      float64
	MaxCoolant    *float64
	SumCoolant    float64
	NCoolant      uint32
	MinBattV      *float64
	SumBattV      float64
	NBattV        uint32
	MaxPackTemp   *float64
	MinSoc        *float64
	HarshEvents   uint32
	OverspeedSmp  uint32
	DTCTotal      uint32
	DTCCooling    uint32
	DTCBattery    uint32
	DTCMisfire    uint32
	DTCEVPack     uint32
	Codes         map[string]uint32
}

// Poisson draws a Poisson variate (Knuth for small λ, normal approx. above).
func Poisson(r *rand.Rand, lambda float64) uint32 {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 {
		v := math.Round(lambda + math.Sqrt(lambda)*r.NormFloat64())
		if v < 0 {
			return 0
		}
		return uint32(v)
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return uint32(k)
		}
		k++
	}
}

func fp(v float64) *float64 { return &v }

// BaseOdometer is the odometer reading at timelineStart.
func (v *Vehicle) BaseOdometer() float64 {
	r := rand.New(rand.NewPCG(v.Seed, 0x0D0))
	return float64(2026-v.Year)*18000*(0.6+0.8*r.Float64()) + 500
}

// OdometerAt approximates the odometer at an absolute day (used by the live
// stream to continue from the back-filled history).
func (v *Vehicle) OdometerAt(day float64) float64 {
	return v.BaseOdometer() + v.DailyKm*0.78*(day-timelineStart)
}

// Daily generates the vehicle's day-level aggregates for absolute day d
// (integer). ok=false when the vehicle was inactive or in the workshop.
func (v *Vehicle) Daily(d int) (DailyRow, bool) {
	r := rand.New(rand.NewPCG(v.Seed^uint64(d)*0x9E3779B97F4A7C15, uint64(d)))
	mid := float64(d) + 0.5
	h := v.HealthAt(mid)
	if h.InRepair {
		return DailyRow{}, false
	}
	day := time.Unix(int64(d)*86400, 0).UTC()
	wd := day.Weekday()
	pActive := 0.9
	if v.Tenant.Kind == "rental" {
		pActive = 0.75
	} else if wd == time.Saturday || wd == time.Sunday {
		pActive = 0.6
	}
	if r.Float64() > pActive {
		return DailyRow{}, false
	}
	km := v.DailyKm * math.Exp(r.NormFloat64()*0.35)
	driveH := km / (28 + 10*r.Float64())
	idleH := driveH * 0.18 * v.IdleBias * math.Exp(r.NormFloat64()*0.3)
	ign := uint32((driveH + idleH) * 3600)
	row := DailyRow{
		TenantID: v.Tenant.ID, VIN: v.VIN, Day: day,
		IgnSamples: ign, IdleSamples: uint32(idleH * 3600), MovingSamples: uint32(driveH * 3600),
		Samples: ign + 600, Codes: map[string]uint32{},
	}
	odoStart := v.OdometerAt(float64(d))
	row.OdoMin, row.OdoMax = odoStart, odoStart+km
	agg := v.Driver.Aggression
	row.MaxSpeed = math.Min(140, 55+30*r.Float64()+12*agg)
	amb := Ambient(v.Depot.City, mid, r)

	if v.Powertrain != "EV" {
		sc := h.Sev[CompCooling]
		mean := 88 + 0.25*(amb-25) + coolantOffset(sc)
		avg := mean + r.NormFloat64()*0.8
		mx := mean + 4 + math.Abs(r.NormFloat64())*2
		if sc > 0.6 {
			mx += r.Float64() * 14 * sc
		}
		if amb > 36 {
			mx += 3 * r.Float64()
		}
		row.MaxCoolant = fp(math.Round(mx*10) / 10)
		row.NCoolant = ign
		row.SumCoolant = avg * float64(ign)
	}
	sb := h.Sev[CompBattery]
	vmean := 14.0 - voltageDrop(sb) - 0.03*float64(2026-v.Year) + r.NormFloat64()*0.08
	vmin := vmean - 0.45 - math.Abs(r.NormFloat64())*0.2
	if sb > 0.5 {
		vmin -= 0.8 * sb * r.Float64()
	}
	row.MinBattV = fp(math.Round(vmin*100) / 100)
	row.NBattV = ign
	row.SumBattV = vmean * float64(ign)
	if v.Powertrain != "ICE" {
		se := h.Sev[CompEVPack]
		row.MaxPackTemp = fp(math.Round((33+0.35*(amb-25)+packTempOffset(se)+math.Abs(r.NormFloat64())*2)*10) / 10)
		if v.Powertrain == "EV" {
			row.MinSoc = fp(math.Max(1, math.Round(15+45*r.Float64()-10*se)))
		}
	}
	row.HarshEvents = Poisson(r, km/100*1.1*agg)
	row.OverspeedSmp = Poisson(r, km/100*18*agg*agg)
	activeH := driveH + idleH
	for _, dr := range dtcRates {
		n := Poisson(r, dr.Rate(h.Sev, v.Powertrain)*activeH)
		if n == 0 {
			continue
		}
		row.Codes[dr.Code] += n
		row.DTCTotal += n
		switch DTCComponent[dr.Code] {
		case CompCooling:
			row.DTCCooling += n
		case CompBattery:
			row.DTCBattery += n
		case CompMisfire:
			row.DTCMisfire += n
		case CompEVPack:
			row.DTCEVPack += n
		}
	}
	return row, true
}

// TripRow is a historical trip (Postgres trip table).
type TripRow struct {
	TripID               string
	StartMs, EndMs       int64
	DistanceKm, MaxSpeed float64
	IdleS, OverspeedS    int64
	HarshBrakes          int
	HarshAccels          int
	StartLat, StartLon   float64
	EndLat, EndLon       float64
	EnergyUsed           float64
}

// TripsFor splits a day's activity into trips during working hours.
func (v *Vehicle) TripsFor(row DailyRow) []TripRow {
	r := rand.New(rand.NewPCG(v.Seed^0x7A1B, uint64(row.Day.Unix())))
	km := row.OdoMax - row.OdoMin
	n := 1 + int(km/55)
	if n > 6 {
		n = 6
	}
	startOfDay := row.Day.Add(time.Duration(6+r.IntN(3)) * time.Hour)
	cursor := startOfDay
	out := make([]TripRow, 0, n)
	lat, lon := v.HomeLat, v.HomeLon
	for i := 0; i < n; i++ {
		share := km / float64(n) * (0.7 + 0.6*r.Float64())
		dur := time.Duration(share/(25+15*r.Float64())*3600) * time.Second
		idle := int64(float64(row.IdleSamples) / float64(n) * (0.5 + r.Float64()))
		dur += time.Duration(idle) * time.Second
		elat, elon := v.HomeLat+r.NormFloat64()*0.05, v.HomeLon+r.NormFloat64()*0.05
		hb := int(Poisson(r, float64(row.HarshEvents)/float64(n)*0.6))
		ha := int(Poisson(r, float64(row.HarshEvents)/float64(n)*0.4))
		out = append(out, TripRow{
			TripID:  DeterministicID("trip", v.VIN+"/"+cursor.Format(time.RFC3339)),
			StartMs: cursor.UnixMilli(), EndMs: cursor.Add(dur).UnixMilli(),
			DistanceKm: math.Round(share*10) / 10, MaxSpeed: math.Round(row.MaxSpeed * (0.7 + 0.3*r.Float64())),
			IdleS: idle, OverspeedS: int64(row.OverspeedSmp) / int64(n), HarshBrakes: hb, HarshAccels: ha,
			StartLat: lat, StartLon: lon, EndLat: elat, EndLon: elon,
			EnergyUsed: math.Round(share/5*10) / 10,
		})
		lat, lon = elat, elon
		cursor = cursor.Add(dur + time.Duration(20+r.IntN(90))*time.Minute)
	}
	return out
}

// HistAlert is a derived historical alert (Postgres alert table).
type HistAlert struct {
	Type, Severity, Title string
	TsMs                  int64
	Details               map[string]any
}

// AlertsFor derives the alerts the real-time engine would have raised on a
// given day, so dashboards have history from the first launch.
func (v *Vehicle) AlertsFor(row DailyRow) []HistAlert {
	r := rand.New(rand.NewPCG(v.Seed^0xA1E7, uint64(row.Day.Unix())))
	at := func() int64 {
		return row.Day.Add(time.Duration(7*3600+r.IntN(12*3600)) * time.Second).UnixMilli()
	}
	var out []HistAlert
	if row.MaxCoolant != nil {
		switch c := *row.MaxCoolant; {
		case c >= 110:
			out = append(out, HistAlert{"ENGINE_OVERHEAT", "CRITICAL", "Engine overheating – stop vehicle safely", at(), map[string]any{"coolant_c": c}})
		case c >= 104:
			out = append(out, HistAlert{"COOLANT_HIGH", "HIGH", "Coolant temperature trending high", at(), map[string]any{"coolant_c": c}})
		}
	}
	if row.MinBattV != nil && *row.MinBattV < 11.8 {
		out = append(out, HistAlert{"LOW_BATTERY_VOLTAGE", "HIGH", "12 V battery/charging system voltage low", at(), map[string]any{"batt_v": *row.MinBattV}})
	}
	if row.MaxPackTemp != nil && *row.MaxPackTemp >= 55 {
		out = append(out, HistAlert{"EV_PACK_OVERTEMP", "CRITICAL", "EV battery pack over-temperature", at(), map[string]any{"pack_temp_c": *row.MaxPackTemp}})
	}
	for code, n := range row.Codes {
		if n == 0 {
			continue
		}
		switch code {
		case "P0217", "P0A80":
			out = append(out, HistAlert{"CRITICAL_DTC", "CRITICAL", dtcTitle(code), at(), map[string]any{"dtc": code, "component": DTCComponent[code]}})
		case "P0480", "P0562", "P0620", "P0300", "P0301", "P0302", "P0A7F", "P0AFA", "U0100":
			out = append(out, HistAlert{"MAJOR_DTC", "MEDIUM", dtcTitle(code), at(), map[string]any{"dtc": code, "component": DTCComponent[code]}})
		}
	}
	if row.HarshEvents >= 6 {
		out = append(out, HistAlert{"RISKY_DRIVING", "MEDIUM", "Repeated harsh driving events", at(), map[string]any{"events": row.HarshEvents}})
	}
	if idleH := float64(row.IdleSamples) / 3600; idleH > 2.5 {
		out = append(out, HistAlert{"EXCESSIVE_IDLE", "LOW", "Excessive idling", at(),
			map[string]any{"idle_min": math.Round(idleH * 60), "est_cost_usd": math.Round(idleH*0.9*1.15*100) / 100}})
	}
	return out
}

var dtcTitles = map[string]string{
	"P0217": "Engine coolant over-temperature condition", "P0A80": "Replace hybrid/EV battery pack",
	"P0480": "Cooling fan 1 control circuit malfunction", "P0562": "System voltage low",
	"P0620": "Generator control circuit malfunction", "P0300": "Random/multiple cylinder misfire detected",
	"P0301": "Cylinder 1 misfire detected", "P0302": "Cylinder 2 misfire detected",
	"P0A7F": "Hybrid/EV battery pack deterioration", "P0AFA": "Hybrid/EV battery system voltage low",
	"U0100": "Lost communication with ECM/PCM",
}

func dtcTitle(c string) string { return dtcTitles[c] }
