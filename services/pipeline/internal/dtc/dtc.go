// Package dtc parses OBD-II Diagnostic Trouble Codes (SAE J2012) from the
// many shapes OEM payloads use, and classifies them for alerting and ML.
package dtc

import (
	"regexp"
	"sort"
	"strings"
)

// Canonical form: system letter, digit 0-3, three hex digits, e.g. P0301.
var canonical = regexp.MustCompile(`^[PCBU][0-3][0-9A-F]{3}$`)

// loose finds DTC-looking tokens inside free text, e.g. "DTC:p0301;P0171 ".
var loose = regexp.MustCompile(`(?i)\b([PCBU])[-_ ]?([0-3][0-9A-F]{3})\b`)

type Severity string

const (
	Critical Severity = "CRITICAL"
	Major    Severity = "MAJOR"
	Minor    Severity = "MINOR"
)

// Component groups codes by the vehicle subsystem the ML model tracks.
type Component string

const (
	Cooling    Component = "cooling"
	Electrical Component = "electrical"
	Misfire    Component = "misfire"
	EVPack     Component = "ev_pack"
	Emissions  Component = "emissions"
	Network    Component = "network"
	Chassis    Component = "chassis"
	Other      Component = "other"
)

type Info struct {
	Code        string    `json:"code"`
	Description string    `json:"description"`
	Severity    Severity  `json:"severity"`
	Component   Component `json:"component"`
}

// Catalogue is the reference table seeded into Postgres (dtc_code).
var Catalogue = map[string]Info{
	"P0115": {"P0115", "Engine coolant temperature circuit malfunction", Major, Cooling},
	"P0116": {"P0116", "Engine coolant temperature circuit range/performance", Minor, Cooling},
	"P0128": {"P0128", "Coolant thermostat below regulating temperature", Minor, Cooling},
	"P0217": {"P0217", "Engine coolant over-temperature condition", Critical, Cooling},
	"P0480": {"P0480", "Cooling fan 1 control circuit malfunction", Major, Cooling},
	"P0300": {"P0300", "Random/multiple cylinder misfire detected", Major, Misfire},
	"P0301": {"P0301", "Cylinder 1 misfire detected", Major, Misfire},
	"P0302": {"P0302", "Cylinder 2 misfire detected", Major, Misfire},
	"P0303": {"P0303", "Cylinder 3 misfire detected", Major, Misfire},
	"P0304": {"P0304", "Cylinder 4 misfire detected", Major, Misfire},
	"P0171": {"P0171", "System too lean (bank 1)", Minor, Emissions},
	"P0420": {"P0420", "Catalyst system efficiency below threshold", Minor, Emissions},
	"P0442": {"P0442", "Evaporative emission system small leak detected", Minor, Emissions},
	"P0456": {"P0456", "Evaporative emission system very small leak", Minor, Emissions},
	"P0562": {"P0562", "System voltage low", Major, Electrical},
	"P0563": {"P0563", "System voltage high", Minor, Electrical},
	"P0620": {"P0620", "Generator control circuit malfunction", Major, Electrical},
	"P0A0F": {"P0A0F", "Engine failed to start", Critical, Electrical},
	"P0A80": {"P0A80", "Replace hybrid/EV battery pack", Critical, EVPack},
	"P0AFA": {"P0AFA", "Hybrid/EV battery system voltage low", Major, EVPack},
	"P0A7F": {"P0A7F", "Hybrid/EV battery pack deterioration", Major, EVPack},
	"P0AA6": {"P0AA6", "Hybrid/EV battery voltage system isolation fault", Critical, EVPack},
	"P0C73": {"P0C73", "Motor electronics coolant pump control performance", Major, EVPack},
	"C0035": {"C0035", "Left front wheel speed sensor circuit", Major, Chassis},
	"C0040": {"C0040", "Right front wheel speed sensor circuit", Major, Chassis},
	"C0750": {"C0750", "Tyre pressure monitor sensor low pressure", Minor, Chassis},
	"B1000": {"B1000", "ECU malfunction (body)", Minor, Other},
	"U0100": {"U0100", "Lost communication with ECM/PCM", Major, Network},
	"U0121": {"U0121", "Lost communication with ABS control module", Major, Network},
}

// Normalize converts raw code tokens such as "p0301", "P-0301" or
// type/number pairs into canonical form. ok is false if not a valid DTC.
// Time O(len(raw)).
func Normalize(raw string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.NewReplacer("-", "", "_", "", " ", "").Replace(s)
	if canonical.MatchString(s) {
		return s, true
	}
	return "", false
}

// FromParts builds a code from a system letter and 4-char number, as sent by
// OEMs that split the two (e.g. {"type":"P","code":"0301"}).
func FromParts(system, number string) (string, bool) {
	return Normalize(system + number)
}

// ExtractAll finds every DTC in a free-form string ("P0301,P0171" or
// "codes=[p0301 p0420]"). Result is de-duplicated and sorted.
// Time O(n log n) in the number of matches.
func ExtractAll(text string) []string {
	matches := loose.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		if c, ok := Normalize(m[1] + m[2]); ok {
			set[c] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Lookup returns catalogue info; unknown but well-formed codes are
// classified MINOR/other so they are stored but do not page anyone.
func Lookup(code string) Info {
	if i, ok := Catalogue[code]; ok {
		return i
	}
	return Info{Code: code, Description: "Unclassified diagnostic trouble code", Severity: Minor, Component: Other}
}

// SystemOf returns the subsystem implied by the leading letter.
func SystemOf(code string) string {
	if code == "" {
		return ""
	}
	switch code[0] {
	case 'P':
		return "powertrain"
	case 'C':
		return "chassis"
	case 'B':
		return "body"
	case 'U':
		return "network"
	}
	return ""
}
