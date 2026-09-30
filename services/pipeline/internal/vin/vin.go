// Package vin validates and generates 17-character Vehicle Identification
// Numbers (ISO 3779 / FMVSS 115 check digit).
package vin

import (
	"errors"
	"fmt"
	"regexp"
)

// vinPattern: 17 chars, digits and capitals, excluding I, O and Q.
var vinPattern = regexp.MustCompile(`^[A-HJ-NPR-Z0-9]{17}$`)

var weights = [17]int{8, 7, 6, 5, 4, 3, 2, 10, 0, 9, 8, 7, 6, 5, 4, 3, 2}

var (
	ErrFormat     = errors.New("vin: must be 17 chars A-Z/0-9 excluding I, O, Q")
	ErrCheckDigit = errors.New("vin: check digit mismatch")
)

// transliterate maps a VIN character to its numeric value.
// O(1) lookup per character.
func transliterate(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'H':
		return int(c-'A') + 1
	case c >= 'J' && c <= 'N':
		return int(c-'J') + 1
	case c == 'P':
		return 7
	case c == 'R':
		return 9
	case c >= 'S' && c <= 'Z':
		return int(c-'S') + 2
	}
	return -1
}

// CheckDigit computes the check digit for the first 17 chars of v (position
// 9 is ignored). Time O(17), space O(1).
func CheckDigit(v string) (byte, error) {
	if len(v) != 17 {
		return 0, ErrFormat
	}
	sum := 0
	for i := 0; i < 17; i++ {
		t := transliterate(v[i])
		if t < 0 {
			return 0, ErrFormat
		}
		sum += t * weights[i]
	}
	r := sum % 11
	if r == 10 {
		return 'X', nil
	}
	return byte('0' + r), nil
}

// Validate checks format and check digit.
func Validate(v string) error {
	if !vinPattern.MatchString(v) {
		return ErrFormat
	}
	cd, err := CheckDigit(v)
	if err != nil {
		return err
	}
	if v[8] != cd {
		return fmt.Errorf("%w: got %c want %c", ErrCheckDigit, v[8], cd)
	}
	return nil
}

// modelYearCodes cycles every 30 years; 2010 = 'A'.
const modelYearCodes = "ABCDEFGHJKLMNPRSTVWXY123456789"

// Build returns a valid VIN from a 3-char WMI, a 5-char vehicle descriptor,
// model year, plant code and a 6-digit serial number.
func Build(wmi, vds string, modelYear int, plant byte, serial int) (string, error) {
	if len(wmi) != 3 || len(vds) != 5 {
		return "", ErrFormat
	}
	yc := modelYearCodes[((modelYear-2010)%30+30)%30]
	b := []byte(fmt.Sprintf("%s%s0%c%c%06d", wmi, vds, yc, plant, serial%1000000))
	cd, err := CheckDigit(string(b))
	if err != nil {
		return "", err
	}
	b[8] = cd
	s := string(b)
	if err := Validate(s); err != nil {
		return "", err
	}
	return s, nil
}
