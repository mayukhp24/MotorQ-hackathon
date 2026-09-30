package sim

import (
	"math"
	"math/rand/v2"

	"fleetpulse/pipeline/internal/canonical"
)

type mode uint8

const (
	modeParked mode = iota
	modeDriving
	modeIdling
	modeCharging
)

// Live is the 1 Hz dynamic state of one simulated vehicle.
type Live struct {
	V           *Vehicle
	r           *rand.Rand
	seq         int64
	lat, lon    float64
	heading     float64
	speed       float64
	target      float64
	odo         float64
	soc, fuel   float64
	coolant     float64
	packTemp    float64
	tyreKpa     float64
	mode        mode
	modeLeft    float64 // seconds remaining in current mode
	excursion   float64 // seconds of overheating excursion remaining
	health      Health
	healthAge   float64
	ambient     float64
	activeShare float64
	day         float64
}

// NewLive initialises live state at absolute time nowSec (unix seconds).
func NewLive(v *Vehicle, nowSec float64, activeShare float64) *Live {
	r := rand.New(rand.NewPCG(v.Seed, uint64(nowSec)))
	day := nowSec / 86400
	l := &Live{
		V: v, r: r, seq: int64(nowSec) * 10, // monotonic across restarts
		lat: v.HomeLat + r.NormFloat64()*0.02, lon: v.HomeLon + r.NormFloat64()*0.02,
		heading: r.Float64() * 360, odo: v.OdometerAt(day),
		soc: 30 + r.Float64()*65, fuel: 20 + r.Float64()*75, tyreKpa: 225 + r.Float64()*15,
		activeShare: activeShare, day: day,
	}
	l.ambient = Ambient(v.Depot.City, day, r)
	l.coolant = l.ambient
	l.packTemp = l.ambient
	l.health = v.HealthAt(day)
	// Start in a mix of modes so the fleet looks alive immediately.
	if r.Float64() < activeShare && !l.health.InRepair {
		l.mode, l.modeLeft = modeDriving, r.ExpFloat64()*1500
		l.speed = 20 + r.Float64()*40
		l.coolant = 85 + r.Float64()*5
	} else {
		l.mode, l.modeLeft = modeParked, r.ExpFloat64()*2400*(1-activeShare)
	}
	return l
}

// Step advances the vehicle by dt seconds to nowSec and returns the sample.
func (l *Live) Step(nowSec, dt float64) canonical.Event {
	v, r := l.V, l.r
	l.healthAge += dt
	if l.healthAge >= 60 {
		l.day = nowSec / 86400
		l.health = v.HealthAt(l.day)
		l.ambient = Ambient(v.Depot.City, l.day, r)
		l.healthAge = 0
	}
	sev := l.health.Sev
	l.modeLeft -= dt
	accel := 0.0
	var events []string

	switch l.mode {
	case modeParked:
		l.speed = 0
		if l.modeLeft <= 0 && !l.health.InRepair {
			if r.Float64() < l.activeShare+0.2 {
				l.mode, l.modeLeft = modeDriving, 300+r.ExpFloat64()*1500
				l.target = 25 + r.Float64()*35
			} else {
				l.modeLeft = 300 + r.ExpFloat64()*1800
			}
		}
	case modeCharging:
		l.speed = 0
		l.soc = math.Min(100, l.soc+0.03*dt)
		if l.soc >= 90 || l.modeLeft <= 0 {
			l.mode, l.modeLeft = modeParked, 600+r.ExpFloat64()*1200
		}
	case modeIdling:
		l.speed = 0
		if l.modeLeft <= 0 {
			l.mode, l.modeLeft = modeDriving, 120+r.ExpFloat64()*900
			l.target = 20 + r.Float64()*50
		}
	case modeDriving:
		agg := v.Driver.Aggression
		if r.Float64() < dt/30 { // new target speed every ~30 s
			hw := r.Float64() < 0.25
			if hw {
				l.target = 60 + r.Float64()*35 + 10*agg
			} else {
				l.target = 15 + r.Float64()*40 + 5*agg
			}
		}
		prev := l.speed
		switch {
		case r.Float64() < 0.00035*agg*agg*dt: // harsh brake
			l.speed = math.Max(0, l.speed-7.5*3.6*dt*(0.8+0.4*r.Float64()))
			if r.Float64() < 0.7 {
				events = append(events, canonical.EvtHarshBrake)
			}
		case r.Float64() < 0.00025*agg*agg*dt && l.speed < 60: // harsh acceleration
			l.speed += 5 * 3.6 * dt
			if r.Float64() < 0.7 {
				events = append(events, canonical.EvtHarshAccel)
			}
		default:
			l.speed += (l.target - l.speed) * math.Min(1, 0.15*dt)
			l.speed = math.Max(0, l.speed+r.NormFloat64()*1.2)
		}
		accel = (l.speed - prev) / 3.6 / dt
		if r.Float64() < 1e-8*dt {
			events = append(events, canonical.EvtCrash)
		}
		// Heading random walk, steering back towards home when far away.
		l.heading += r.NormFloat64() * 4
		dLat, dLon := v.HomeLat-l.lat, v.HomeLon-l.lon
		if dLat*dLat+dLon*dLon > 0.02 { // ≈ 15 km
			want := math.Atan2(dLon, dLat) * 180 / math.Pi
			l.heading += angleDiff(want, l.heading) * 0.1
		}
		l.heading = math.Mod(l.heading+360, 360)
		dist := l.speed / 3.6 * dt
		rad := l.heading * math.Pi / 180
		l.lat += dist * math.Cos(rad) / 111320
		l.lon += dist * math.Sin(rad) / (111320 * math.Cos(l.lat*math.Pi/180))
		l.odo += dist / 1000
		km := dist / 1000
		if v.Powertrain == "EV" {
			l.soc = math.Max(0, l.soc-km*v.Model.KwhPer100/100/v.Model.BatteryKwh*100*(1+0.3*sev[CompEVPack]))
		} else {
			l.fuel = math.Max(0, l.fuel-km*v.Model.LPer100/100/v.Model.TankL*100)
		}
		if r.Float64() < dt/240 { // stop at a light / delivery
			long := r.Float64() < 0.04*v.IdleBias
			l.mode, l.modeLeft = modeIdling, 20+r.Float64()*90
			if long {
				l.modeLeft = 600 + r.Float64()*900
			}
		}
		if l.modeLeft <= 0 {
			l.mode, l.modeLeft = modeParked, 600+r.ExpFloat64()*2400*(1-l.activeShare)
			if v.Powertrain == "EV" && l.soc < 35 {
				l.mode, l.modeLeft = modeCharging, 3600
			}
			if v.Powertrain != "EV" && l.fuel < 15 {
				l.fuel = 90 + r.Float64()*10
			}
		}
	}
	if l.health.InRepair && l.mode != modeParked {
		l.mode, l.speed = modeParked, 0
	}
	ign := l.mode == modeDriving || l.mode == modeIdling

	// Thermal model: first-order approach to the operating/ambient temperature.
	if v.Powertrain != "EV" {
		target := l.ambient
		if ign {
			target = 88 + 0.25*(l.ambient-25) + coolantOffset(sev[CompCooling])
			if l.excursion > 0 {
				l.excursion -= dt
				target += 22
			} else if sc := sev[CompCooling]; sc > 0.6 && r.Float64() < 0.0015*sc*dt {
				l.excursion = 60 + r.Float64()*150
			}
		}
		l.coolant += (target - l.coolant) * math.Min(1, dt/90)
	}
	if v.Powertrain != "ICE" {
		target := l.ambient + 3
		if ign || l.mode == modeCharging {
			target = 30 + 0.35*(l.ambient-25) + packTempOffset(sev[CompEVPack]) + l.speed*0.03
		}
		l.packTemp += (target - l.packTemp) * math.Min(1, dt/300)
	}

	e := canonical.Event{
		VIN: v.VIN, OEM: v.OEM, TsMs: int64(nowSec * 1000), Seq: l.nextSeq(),
		Lat: round6(l.lat), Lon: round6(l.lon), Heading: math.Round(l.heading), SpeedKmh: round1(l.speed),
		AccelMps2: round2(accel), OdoKm: round3(l.odo), Ignition: ign, Charging: l.mode == modeCharging,
		Events: events,
	}
	battV := 12.6 - 0.5*sev[CompBattery] + r.NormFloat64()*0.03
	if ign {
		battV = 14.0 - voltageDrop(sev[CompBattery]) - 0.03*float64(2026-v.Year) + r.NormFloat64()*0.06
	}
	e.BattV = canonical.F(round2(battV))
	e.TyreKpa = canonical.F(round1(l.tyreKpa + r.NormFloat64()*0.5))
	if v.Powertrain != "EV" {
		e.CoolantC = canonical.F(round1(l.coolant + r.NormFloat64()*0.3))
		e.FuelPct = canonical.F(round1(l.fuel))
		rpm, load := 0.0, 0.0
		if ign {
			rpm = 750 + l.speed*28 + r.NormFloat64()*(20+300*sev[CompMisfire])
			load = math.Min(100, 18+l.speed*0.45+math.Max(0, accel)*8)
		}
		e.RPM, e.EngLoadPct = canonical.F(math.Round(math.Max(0, rpm))), canonical.F(round1(load))
	}
	if v.Powertrain != "ICE" {
		e.PackTempC = canonical.F(round1(l.packTemp))
		if v.Powertrain == "EV" {
			e.SocPct = canonical.F(round1(l.soc))
		}
	}
	// Discrete diagnostic events (rates are per active hour).
	if ign {
		for _, dr := range dtcRates {
			if p := dr.Rate(sev, v.Powertrain) * dt / 3600; p > 0 && r.Float64() < p {
				e.DTC = append(e.DTC, dr.Code)
			}
		}
		if l.coolant > 112 && r.Float64() < 0.02*dt {
			e.DTC = append(e.DTC, "P0217")
		}
	}
	return e
}

func (l *Live) nextSeq() int64 { l.seq++; return l.seq }

// Mode returns the current mode name (for tests/metrics).
func (l *Live) Mode() string {
	return [...]string{"PARKED", "DRIVING", "IDLING", "CHARGING"}[l.mode]
}

func angleDiff(a, b float64) float64 {
	d := math.Mod(a-b+540, 360) - 180
	return d
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
