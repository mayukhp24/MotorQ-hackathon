package stream

const base32 = "0123456789bcdefghjkmnpqrstuvwxyz"

// Geohash encodes a coordinate into a base-32 string of the given precision
// by interleaving longitude/latitude bisection bits. Nearby points share
// prefixes, so a prefix is a spatial bucket (precision 5 ≈ 4.9 km cells).
// Time O(precision), space O(precision).
func Geohash(lat, lon float64, precision int) string {
	if precision <= 0 {
		return ""
	}
	latLo, latHi := -90.0, 90.0
	lonLo, lonHi := -180.0, 180.0
	out := make([]byte, 0, precision)
	var ch, bit int
	even := true
	for len(out) < precision {
		if even {
			mid := (lonLo + lonHi) / 2
			if lon >= mid {
				ch |= 1 << (4 - bit)
				lonLo = mid
			} else {
				lonHi = mid
			}
		} else {
			mid := (latLo + latHi) / 2
			if lat >= mid {
				ch |= 1 << (4 - bit)
				latLo = mid
			} else {
				latHi = mid
			}
		}
		even = !even
		if bit < 4 {
			bit++
		} else {
			out = append(out, base32[ch])
			bit, ch = 0, 0
		}
	}
	return string(out)
}

// GeohashCenter decodes a geohash to the centre of its cell.
func GeohashCenter(h string) (lat, lon float64) {
	latLo, latHi := -90.0, 90.0
	lonLo, lonHi := -180.0, 180.0
	even := true
	for i := 0; i < len(h); i++ {
		idx := -1
		for j := 0; j < 32; j++ {
			if base32[j] == h[i] {
				idx = j
				break
			}
		}
		if idx < 0 {
			return 0, 0
		}
		for b := 4; b >= 0; b-- {
			set := idx&(1<<b) != 0
			if even {
				mid := (lonLo + lonHi) / 2
				if set {
					lonLo = mid
				} else {
					lonHi = mid
				}
			} else {
				mid := (latLo + latHi) / 2
				if set {
					latLo = mid
				} else {
					latHi = mid
				}
			}
			even = !even
		}
	}
	return (latLo + latHi) / 2, (lonLo + lonHi) / 2
}
