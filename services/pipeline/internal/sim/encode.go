package sim

import (
	"strconv"
	"strings"
	"time"

	"fleetpulse/pipeline/internal/canonical"
)

// Encoders write one canonical sample in an OEM's native wire format. They
// are the inverse of the ingest adapters and are hand-rolled (append-style,
// no reflection) so the simulator can emit 100K+ records/s.

type Encoder interface {
	Begin(buf []byte) []byte
	Record(buf []byte, e *canonical.Event, first bool) []byte
	End(buf []byte) []byte
}

func EncoderFor(oem string) Encoder {
	switch oem {
	case "pinnacle":
		return pinnacleEnc{}
	case "stellar":
		return stellarEnc{}
	default:
		return auroraEnc{}
	}
}

func af(b []byte, v float64, prec int) []byte { return strconv.AppendFloat(b, v, 'f', prec, 64) }
func ai(b []byte, v int64) []byte             { return strconv.AppendInt(b, v, 10) }
func astr(b []byte, s string) []byte          { return strconv.AppendQuote(b, s) }

func strList(b []byte, xs []string) []byte {
	b = append(b, '[')
	for i, x := range xs {
		if i > 0 {
			b = append(b, ',')
		}
		b = astr(b, x)
	}
	return append(b, ']')
}

// ---- aurora: nested JSON, metric units, epoch-ms timestamps.
type auroraEnc struct{}

func (auroraEnc) Begin(b []byte) []byte { return append(b, `{"oem":"aurora","records":[`...) }
func (auroraEnc) End(b []byte) []byte   { return append(b, "]}"...) }
func (auroraEnc) Record(b []byte, e *canonical.Event, first bool) []byte {
	if !first {
		b = append(b, ',')
	}
	b = append(b, `{"vehicle":{"vin":`...)
	b = astr(b, e.VIN)
	b = append(b, `},"timestamp":`...)
	b = ai(b, e.TsMs)
	b = append(b, `,"sequence":`...)
	b = ai(b, e.Seq)
	b = append(b, `,"position":{"latitude":`...)
	b = af(b, e.Lat, 6)
	b = append(b, `,"longitude":`...)
	b = af(b, e.Lon, 6)
	b = append(b, `,"heading":`...)
	b = af(b, e.Heading, 0)
	b = append(b, `},"motion":{"speedKph":`...)
	b = af(b, e.SpeedKmh, 1)
	b = append(b, `,"accelMps2":`...)
	b = af(b, e.AccelMps2, 2)
	b = append(b, `},"powertrain":{"odometerKm":`...)
	b = af(b, e.OdoKm, 3)
	b = append(b, `,"engineOn":`...)
	b = strconv.AppendBool(b, e.Ignition)
	if e.RPM != nil {
		b = append(b, `,"rpm":`...)
		b = af(b, *e.RPM, 0)
	}
	if e.CoolantC != nil {
		b = append(b, `,"coolantTempC":`...)
		b = af(b, *e.CoolantC, 1)
	}
	if e.EngLoadPct != nil {
		b = append(b, `,"engineLoadPct":`...)
		b = af(b, *e.EngLoadPct, 1)
	}
	if e.FuelPct != nil {
		b = append(b, `,"fuelLevelPct":`...)
		b = af(b, *e.FuelPct, 1)
	}
	b = append(b, '}')
	if e.SocPct != nil || e.PackTempC != nil {
		b = append(b, `,"ev":{"charging":`...)
		b = strconv.AppendBool(b, e.Charging)
		if e.SocPct != nil {
			b = append(b, `,"socPct":`...)
			b = af(b, *e.SocPct, 1)
		}
		if e.PackTempC != nil {
			b = append(b, `,"packTempC":`...)
			b = af(b, *e.PackTempC, 1)
		}
		b = append(b, '}')
	}
	if e.BattV != nil {
		b = append(b, `,"electrical":{"batteryVoltage":`...)
		b = af(b, *e.BattV, 2)
		b = append(b, '}')
	}
	if e.TyreKpa != nil {
		b = append(b, `,"chassis":{"tyreMinKpa":`...)
		b = af(b, *e.TyreKpa, 1)
		b = append(b, '}')
	}
	if len(e.DTC) > 0 {
		b = append(b, `,"diagnostics":{"dtcs":`...)
		b = strList(b, e.DTC)
		b = append(b, '}')
	}
	if len(e.Events) > 0 {
		b = append(b, `,"events":`...)
		b = strList(b, e.Events)
	}
	return append(b, '}')
}

// ---- pinnacle: flat, imperial units, ISO-8601 time, CSV code strings.
type pinnacleEnc struct{}

var pinnacleEvt = map[string]string{canonical.EvtHarshBrake: "HB", canonical.EvtHarshAccel: "HA", canonical.EvtHarshTurn: "HC", canonical.EvtCrash: "CR"}

func (pinnacleEnc) Begin(b []byte) []byte { return append(b, `{"data":[`...) }
func (pinnacleEnc) End(b []byte) []byte   { return append(b, "]}"...) }
func (pinnacleEnc) Record(b []byte, e *canonical.Event, first bool) []byte {
	if !first {
		b = append(b, ',')
	}
	b = append(b, `{"VIN":`...)
	b = astr(b, e.VIN)
	b = append(b, `,"EventTime":"`...)
	b = time.UnixMilli(e.TsMs).UTC().AppendFormat(b, "2006-01-02T15:04:05.000Z07:00")
	b = append(b, `","Seq":`...)
	b = ai(b, e.Seq)
	b = append(b, `,"Lat":`...)
	b = af(b, e.Lat, 6)
	b = append(b, `,"Lng":`...)
	b = af(b, e.Lon, 6)
	b = append(b, `,"Hdg":`...)
	b = af(b, e.Heading, 0)
	b = append(b, `,"SpeedMph":`...)
	b = af(b, e.SpeedKmh/1.609344, 3)
	b = append(b, `,"AccelG":`...)
	b = af(b, e.AccelMps2/9.80665, 4)
	b = append(b, `,"OdometerMi":`...)
	b = af(b, e.OdoKm/1.609344, 4)
	if e.Ignition {
		b = append(b, `,"Ign":"ON"`...)
	} else {
		b = append(b, `,"Ign":"OFF"`...)
	}
	if e.Charging {
		b = append(b, `,"Chg":"Y"`...)
	} else {
		b = append(b, `,"Chg":"N"`...)
	}
	if e.RPM != nil {
		b = append(b, `,"RPM":`...)
		b = af(b, *e.RPM, 0)
	}
	if e.CoolantC != nil {
		b = append(b, `,"CoolantF":`...)
		b = af(b, *e.CoolantC*9/5+32, 2)
	}
	if e.EngLoadPct != nil {
		b = append(b, `,"LoadPct":`...)
		b = af(b, *e.EngLoadPct, 1)
	}
	if e.FuelPct != nil {
		b = append(b, `,"FuelPct":`...)
		b = af(b, *e.FuelPct, 1)
	}
	if e.SocPct != nil {
		b = append(b, `,"SOC":`...)
		b = af(b, *e.SocPct/100, 4)
	}
	if e.PackTempC != nil {
		b = append(b, `,"PackTempF":`...)
		b = af(b, *e.PackTempC*9/5+32, 2)
	}
	if e.BattV != nil {
		b = append(b, `,"BattV":`...)
		b = af(b, *e.BattV, 2)
	}
	if e.TyreKpa != nil {
		b = append(b, `,"TireMinPsi":`...)
		b = af(b, *e.TyreKpa/6.894757, 3)
	}
	if len(e.DTC) > 0 {
		b = append(b, `,"DTC":`...)
		b = astr(b, strings.Join(e.DTC, ","))
	}
	if len(e.Events) > 0 {
		codes := make([]string, 0, len(e.Events))
		for _, ev := range e.Events {
			codes = append(codes, pinnacleEvt[ev])
		}
		b = append(b, `,"Evt":`...)
		b = astr(b, strings.Join(codes, "|"))
	}
	return append(b, '}')
}

// ---- stellar: signal list with per-signal units, epoch seconds.
type stellarEnc struct{}

var stellarEvt = map[string]string{canonical.EvtHarshBrake: "harshBraking", canonical.EvtHarshAccel: "harshAcceleration", canonical.EvtHarshTurn: "harshCornering", canonical.EvtCrash: "collision"}

func (stellarEnc) Begin(b []byte) []byte { return append(b, `{"messages":[`...) }
func (stellarEnc) End(b []byte) []byte   { return append(b, "]}"...) }

func sig(b []byte, name string, v float64, prec int, unit string) []byte {
	b = append(b, `,{"name":"`...)
	b = append(b, name...)
	b = append(b, `","value":`...)
	b = af(b, v, prec)
	if unit != "" {
		b = append(b, `,"unit":"`...)
		b = append(b, unit...)
		b = append(b, '"')
	}
	return append(b, '}')
}

func (stellarEnc) Record(b []byte, e *canonical.Event, first bool) []byte {
	if !first {
		b = append(b, ',')
	}
	b = append(b, `{"vin":`...)
	b = astr(b, e.VIN)
	b = append(b, `,"t":`...)
	b = af(b, float64(e.TsMs)/1000, 3)
	b = append(b, `,"n":`...)
	b = ai(b, e.Seq)
	b = append(b, `,"signals":[{"name":"gps.lat","value":`...)
	b = af(b, e.Lat, 6)
	b = append(b, '}')
	b = sig(b, "gps.lon", e.Lon, 6, "")
	b = sig(b, "gps.heading", e.Heading, 0, "")
	b = sig(b, "speed", e.SpeedKmh/3.6, 3, "m/s")
	b = sig(b, "accel.long", e.AccelMps2, 2, "m/s2")
	b = sig(b, "odometer", e.OdoKm*1000, 0, "m")
	ign := 0.0
	if e.Ignition {
		ign = 1
	}
	b = sig(b, "ignition", ign, 0, "")
	if e.RPM != nil {
		b = sig(b, "rpm", *e.RPM, 0, "")
	}
	if e.CoolantC != nil {
		b = sig(b, "coolant.temp", *e.CoolantC+273.15, 2, "K")
	}
	if e.EngLoadPct != nil {
		b = sig(b, "engine.load", *e.EngLoadPct, 1, "%")
	}
	if e.FuelPct != nil {
		b = sig(b, "fuel.level", *e.FuelPct, 1, "%")
	}
	if e.SocPct != nil {
		b = sig(b, "hv.soc", *e.SocPct, 1, "%")
	}
	if e.PackTempC != nil {
		b = sig(b, "hv.temp", *e.PackTempC, 1, "C")
	}
	if e.SocPct != nil || e.PackTempC != nil {
		chg := 0.0
		if e.Charging {
			chg = 1
		}
		b = sig(b, "hv.charging", chg, 0, "")
	}
	if e.BattV != nil {
		b = sig(b, "lv.voltage", *e.BattV, 2, "V")
	}
	if e.TyreKpa != nil {
		b = sig(b, "tpms.min", *e.TyreKpa, 1, "kPa")
	}
	b = append(b, ']')
	if len(e.DTC) > 0 {
		b = append(b, `,"faults":[`...)
		for i, c := range e.DTC {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"type":"`...)
			b = append(b, c[0])
			b = append(b, `","code":"`...)
			b = append(b, c[1:]...)
			b = append(b, `"}`...)
		}
		b = append(b, ']')
	}
	if len(e.Events) > 0 {
		names := make([]string, 0, len(e.Events))
		for _, ev := range e.Events {
			names = append(names, stellarEvt[ev])
		}
		b = append(b, `,"alerts":`...)
		b = strList(b, names)
	}
	return append(b, '}')
}
