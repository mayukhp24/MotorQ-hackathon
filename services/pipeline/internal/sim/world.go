// Package sim generates a synthetic but internally consistent connected-fleet
// world: tenants, depots, fleets, drivers and 100K+ vehicles, each with a
// hidden component-health process. The same model drives (a) the historical
// back-fill used to train the predictive-maintenance model and (b) the live
// 1 Hz telemetry stream, so what the model learns is what the stream shows.
package sim

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/google/uuid"

	"fleetpulse/pipeline/internal/vin"
)

var ns = uuid.MustParse("0b6f3a52-6e1a-4d3c-9f56-3f1b8a6d2c77")

// DeterministicID derives stable UUIDs from names so every component (seed,
// processor, API) agrees on identifiers without coordination.
func DeterministicID(kind, name string) string {
	return uuid.NewSHA1(ns, []byte(kind+"|"+name)).String()
}

type City struct {
	Name  string
	State string // registration prefix
	Lat   float64
	Lon   float64
	// Seasonal ambient temperature (°C): mean and amplitude.
	TempMean, TempAmp float64
}

var Cities = []City{
	{"Chennai", "TN", 13.0827, 80.2707, 30, 4},
	{"Bengaluru", "KA", 12.9716, 77.5946, 25, 3},
	{"Mumbai", "MH", 19.0760, 72.8777, 28, 3},
	{"Delhi", "DL", 28.6139, 77.2090, 25, 10},
	{"Hyderabad", "TS", 17.3850, 78.4867, 28, 6},
	{"Pune", "MH", 18.5204, 73.8567, 26, 5},
	{"Ahmedabad", "GJ", 23.0225, 72.5714, 29, 7},
	{"Kolkata", "WB", 22.5726, 88.3639, 27, 6},
	{"Surat", "GJ", 21.1702, 72.8311, 28, 5},
	{"Jaipur", "RJ", 26.9124, 75.7873, 26, 9},
}

type Tenant struct {
	ID, Slug, Name, Plan, Kind string
	Share                      float64 // share of total vehicles
	EVShare, HEVShare          float64
	CityIdx                    []int
	DailyKmMean                float64
}

type Depot struct {
	ID, TenantID, Name string
	City               City
	Lat, Lon           float64
}

type Fleet struct {
	ID, TenantID, DepotID, Name string
}

type Model struct {
	ID, OEM, Name, VDS, Powertrain, BodyType string
	BatteryKwh, TankL                        float64
	KwhPer100, LPer100                       float64
}

type OEM struct {
	Code, Name, WMI string
}

var OEMs = []OEM{
	{"aurora", "Aurora Motors", "AUR"},
	{"pinnacle", "Pinnacle Automotive", "PNC"},
	{"stellar", "Stellar Mobility Group", "STL"},
}

var Models = []Model{
	{OEM: "aurora", Name: "Aurora Cargo D", VDS: "CGD4X", Powertrain: "ICE", BodyType: "van", TankL: 80, LPer100: 10.5},
	{OEM: "aurora", Name: "Aurora e-Cargo", VDS: "ECG7E", Powertrain: "EV", BodyType: "van", BatteryKwh: 75, KwhPer100: 22},
	{OEM: "aurora", Name: "Aurora Hybrid Sedan", VDS: "HYS2H", Powertrain: "HEV", BodyType: "sedan", TankL: 45, LPer100: 4.8, BatteryKwh: 1.6},
	{OEM: "pinnacle", Name: "Pinnacle Trailhead", VDS: "TRH5D", Powertrain: "ICE", BodyType: "suv", TankL: 60, LPer100: 9.2},
	{OEM: "pinnacle", Name: "Pinnacle Metro", VDS: "MTR3P", Powertrain: "ICE", BodyType: "sedan", TankL: 45, LPer100: 6.5},
	{OEM: "pinnacle", Name: "Pinnacle Solace EV", VDS: "SLC8E", Powertrain: "EV", BodyType: "suv", BatteryKwh: 64, KwhPer100: 17},
	{OEM: "stellar", Name: "Stellar Duro", VDS: "DRX6D", Powertrain: "ICE", BodyType: "truck", TankL: 120, LPer100: 16},
	{OEM: "stellar", Name: "Stellar e-Duro", VDS: "EDR9E", Powertrain: "EV", BodyType: "truck", BatteryKwh: 110, KwhPer100: 32},
	{OEM: "stellar", Name: "Stellar Volt Hybrid", VDS: "VLT4H", Powertrain: "HEV", BodyType: "hatchback", TankL: 40, LPer100: 4.2, BatteryKwh: 1.3},
}

var DefaultTenants = []Tenant{
	{Slug: "acme-logistics", Name: "Acme Logistics", Plan: "enterprise", Kind: "logistics", Share: 0.55, EVShare: 0.10, HEVShare: 0.10, CityIdx: []int{0, 1, 2, 3, 4, 5, 6, 7}, DailyKmMean: 190},
	{Slug: "rapidride-rentals", Name: "RapidRide Rentals", Plan: "business", Kind: "rental", Share: 0.30, EVShare: 0.25, HEVShare: 0.15, CityIdx: []int{1, 2, 3, 4, 8}, DailyKmMean: 95},
	{Slug: "greenfleet-ev", Name: "GreenFleet EV", Plan: "business", Kind: "last-mile", Share: 0.15, EVShare: 0.85, HEVShare: 0.10, CityIdx: []int{0, 1, 5, 9}, DailyKmMean: 120},
}

type Driver struct {
	ID, TenantID, Name, LicenseNo, Phone string
	Aggression                           float64
}

type Vehicle struct {
	Idx        int
	VIN        string
	Tenant     *Tenant
	Fleet      *Fleet
	Depot      *Depot
	Model      *Model
	OEM        string
	Powertrain string
	Year       int
	Plate      string
	Driver     *Driver
	HomeLat    float64
	HomeLon    float64
	DailyKm    float64
	IdleBias   float64
	Episodes   []Episode
	Seed       uint64
}

type World struct {
	Seed     uint64
	Tenants  []*Tenant
	Depots   []*Depot
	Fleets   []*Fleet
	Models   []*Model
	Drivers  []*Driver
	Vehicles []*Vehicle
	byVIN    map[string]*Vehicle
}

var firstNames = []string{"Aarav", "Vivaan", "Aditya", "Vihaan", "Arjun", "Sai", "Reyansh", "Ayaan", "Krishna", "Ishaan",
	"Ananya", "Diya", "Aadhya", "Saanvi", "Kavya", "Meera", "Priya", "Riya", "Lakshmi", "Nisha",
	"Rahul", "Karthik", "Suresh", "Ramesh", "Imran", "Farhan", "Joseph", "Thomas", "Harpreet", "Gurpreet"}
var lastNames = []string{"Sharma", "Iyer", "Reddy", "Nair", "Patel", "Khan", "Singh", "Gupta", "Das", "Menon",
	"Rao", "Pillai", "Joshi", "Kulkarni", "Chatterjee", "Banerjee", "Fernandes", "Mehta", "Verma", "Naidu"}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

// BuildWorld deterministically generates the reference world.
func BuildWorld(seed uint64, vehicles int) (*World, error) {
	r := rand.New(rand.NewPCG(seed, 0xF1EE7))
	w := &World{Seed: seed, byVIN: make(map[string]*Vehicle, vehicles)}
	for i := range Models {
		m := Models[i]
		m.ID = DeterministicID("model", m.OEM+"/"+m.Name)
		w.Models = append(w.Models, &m)
	}
	modelsBy := map[string][]*Model{} // key oem|powertrain
	for _, m := range w.Models {
		k := m.OEM + "|" + m.Powertrain
		modelsBy[k] = append(modelsBy[k], m)
	}
	for i := range DefaultTenants {
		t := DefaultTenants[i]
		t.ID = DeterministicID("tenant", t.Slug)
		w.Tenants = append(w.Tenants, &t)
		for _, ci := range t.CityIdx {
			c := Cities[ci]
			d := &Depot{ID: DeterministicID("depot", t.Slug+"/"+c.Name), TenantID: t.ID,
				Name: fmt.Sprintf("%s %s Hub", t.Name, c.Name), City: c,
				Lat: c.Lat + r.NormFloat64()*0.03, Lon: c.Lon + r.NormFloat64()*0.03}
			w.Depots = append(w.Depots, d)
			w.Fleets = append(w.Fleets, &Fleet{ID: DeterministicID("fleet", t.Slug+"/"+c.Name), TenantID: t.ID,
				DepotID: d.ID, Name: fmt.Sprintf("%s – %s", t.Name, c.Name)})
		}
	}
	// Index depots/fleets per tenant.
	type tf struct {
		d *Depot
		f *Fleet
	}
	byTenant := map[string][]tf{}
	for i, d := range w.Depots {
		byTenant[d.TenantID] = append(byTenant[d.TenantID], tf{d, w.Fleets[i]})
	}

	counts := make([]int, len(w.Tenants))
	total := 0
	for i, t := range w.Tenants {
		counts[i] = int(math.Round(t.Share * float64(vehicles)))
		total += counts[i]
	}
	counts[0] += vehicles - total

	idx := 0
	for ti, t := range w.Tenants {
		// ~0.8 drivers per vehicle (shift sharing / spare vehicles).
		nDrivers := int(float64(counts[ti])*0.8) + 1
		drivers := make([]*Driver, nDrivers)
		for d := 0; d < nDrivers; d++ {
			name := pick(r, firstNames) + " " + pick(r, lastNames)
			drivers[d] = &Driver{
				ID:         DeterministicID("driver", fmt.Sprintf("%s/%d", t.Slug, d)),
				TenantID:   t.ID,
				Name:       name,
				LicenseNo:  fmt.Sprintf("DL-%02d-%011d", r.IntN(40), r.Int64N(1e11)),
				Phone:      fmt.Sprintf("+91-9%09d", r.Int64N(1e9)),
				Aggression: math.Min(4, math.Exp(r.NormFloat64()*0.45)),
			}
		}
		w.Drivers = append(w.Drivers, drivers...)
		sites := byTenant[t.ID]
		for k := 0; k < counts[ti]; k++ {
			site := sites[r.IntN(len(sites))]
			pt := "ICE"
			switch x := r.Float64(); {
			case x < t.EVShare:
				pt = "EV"
			case x < t.EVShare+t.HEVShare:
				pt = "HEV"
			}
			oem := OEMs[r.IntN(len(OEMs))]
			cands := modelsBy[oem.Code+"|"+pt]
			for len(cands) == 0 { // OEM lacks this powertrain: pick another OEM
				oem = OEMs[r.IntN(len(OEMs))]
				cands = modelsBy[oem.Code+"|"+pt]
			}
			m := cands[r.IntN(len(cands))]
			year := 2018 + r.IntN(8)
			v, err := vin.Build(oem.WMI, m.VDS, year, "ABCDEFGH"[idx%8], idx)
			if err != nil {
				return nil, err
			}
			veh := &Vehicle{
				Idx: idx, VIN: v, Tenant: t, Fleet: site.f, Depot: site.d, Model: m, OEM: oem.Code,
				Powertrain: pt, Year: year,
				Plate:   fmt.Sprintf("%s%02d%c%c%04d", site.d.City.State, 1+r.IntN(40), 'A'+rune(r.IntN(26)), 'A'+rune(r.IntN(26)), idx%10000),
				Driver:  drivers[r.IntN(nDrivers)],
				HomeLat: site.d.Lat + r.NormFloat64()*0.02, HomeLon: site.d.Lon + r.NormFloat64()*0.02,
				DailyKm:  t.DailyKmMean * math.Exp(r.NormFloat64()*0.3),
				IdleBias: math.Exp(r.NormFloat64() * 0.5),
				Seed:     r.Uint64(),
			}
			veh.Episodes = GenerateEpisodes(veh)
			w.Vehicles = append(w.Vehicles, veh)
			w.byVIN[v] = veh
			idx++
		}
	}
	return w, nil
}

// ByVIN looks up a vehicle.
func (w *World) ByVIN(v string) *Vehicle { return w.byVIN[v] }
