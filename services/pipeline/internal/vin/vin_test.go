package vin

import (
	"errors"
	"testing"
)

func TestValidate_KnownGoodVINs(t *testing.T) {
	good := []string{
		"1HGCM82633A004352", // example from the case study
		"1M8GDM9AXKP042788", // classic check digit X example
		"11111111111111111",
	}
	for _, v := range good {
		if err := Validate(v); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", v, err)
		}
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := map[string]error{
		"1HGCM82633A00435":   ErrFormat,     // 16 chars
		"1HGCM82633A0043521": ErrFormat,     // 18 chars
		"1HGCM82633A00435I":  ErrFormat,     // contains I
		"1HGCM82O33A004352":  ErrFormat,     // contains O
		"1hgcm82633a004352":  ErrFormat,     // lower case
		"1HGCM82643A004352":  ErrCheckDigit, // wrong check digit
	}
	for v, want := range cases {
		if err := Validate(v); !errors.Is(err, want) {
			t.Errorf("Validate(%q) = %v, want %v", v, err, want)
		}
	}
}

func TestBuild_ProducesValidUniqueVINs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		v, err := Build("AUR", "FL3E2", 2018+i%8, 'C', i)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if err := Validate(v); err != nil {
			t.Fatalf("built VIN %s invalid: %v", v, err)
		}
		if seen[v] {
			t.Fatalf("duplicate VIN %s", v)
		}
		seen[v] = true
	}
}

func TestBuild_BadInput(t *testing.T) {
	if _, err := Build("AU", "FL3E2", 2020, 'C', 1); err == nil {
		t.Fatal("expected error for short WMI")
	}
	if _, err := Build("AUI", "FL3E2", 2020, 'C', 1); err == nil {
		t.Fatal("expected error for WMI containing I")
	}
}

func TestCheckDigit_BadLength(t *testing.T) {
	if _, err := CheckDigit("ABC"); err == nil {
		t.Fatal("expected error")
	}
}

func BenchmarkValidate(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Validate("1HGCM82633A004352")
	}
}
