package dtc

import (
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"P0301", "P0301", true},
		{"p0301", "P0301", true},
		{" P-0301 ", "P0301", true},
		{"u0100", "U0100", true},
		{"P0A80", "P0A80", true},
		{"P4301", "", false}, // second digit must be 0-3
		{"X0301", "", false},
		{"P030", "", false},
		{"P03011", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := Normalize(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("Normalize(%q) = (%q,%v) want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFromParts(t *testing.T) {
	if c, ok := FromParts("P", "0301"); !ok || c != "P0301" {
		t.Fatalf("got %q %v", c, ok)
	}
	if _, ok := FromParts("Z", "0301"); ok {
		t.Fatal("expected invalid")
	}
}

func TestExtractAll(t *testing.T) {
	got := ExtractAll("DTC:p0301;P0171, P-0301 junk Q1234 codes=[u0100]")
	want := []string{"P0171", "P0301", "U0100"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractAll = %v want %v", got, want)
	}
	if ExtractAll("no codes here") != nil {
		t.Fatal("expected nil")
	}
}

func TestLookup(t *testing.T) {
	if Lookup("P0217").Severity != Critical {
		t.Fatal("P0217 should be critical")
	}
	u := Lookup("P1234")
	if u.Severity != Minor || u.Component != Other {
		t.Fatalf("unknown code classification wrong: %+v", u)
	}
}

func TestCatalogueIsCanonical(t *testing.T) {
	for k, v := range Catalogue {
		if k != v.Code {
			t.Errorf("key %s != code %s", k, v.Code)
		}
		if _, ok := Normalize(k); !ok {
			t.Errorf("catalogue code %s not canonical", k)
		}
	}
}

func TestSystemOf(t *testing.T) {
	for code, want := range map[string]string{"P0301": "powertrain", "C0035": "chassis", "B1000": "body", "U0100": "network", "": "", "Z": ""} {
		if got := SystemOf(code); got != want {
			t.Errorf("SystemOf(%q)=%q want %q", code, got, want)
		}
	}
}
