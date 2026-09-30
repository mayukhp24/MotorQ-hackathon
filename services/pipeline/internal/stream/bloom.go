// Package stream holds the bounded-memory streaming data structures used on
// the hot path: Bloom-filter de-duplication, Count-Min Sketch heavy hitters
// and geohash spatial bucketing.
package stream

import (
	"hash/maphash"
	"math"
	"sync"
	"time"
)

// Bloom is a standard Bloom filter using Kirsch–Mitzenmacher double hashing:
// k indexes are derived from two 64-bit hashes, h1 + i*h2.
// Add/Test: O(k) time; space m bits.
type Bloom struct {
	bits []uint64
	m    uint64
	k    uint64
	seed maphash.Seed
}

// NewBloom sizes a filter for n items at false-positive rate p:
// m = -n ln p / (ln 2)^2, k = (m/n) ln 2.
func NewBloom(n int, p float64) *Bloom {
	if n < 1 {
		n = 1
	}
	if p <= 0 || p >= 1 {
		p = 0.001
	}
	m := uint64(math.Ceil(-float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)))
	k := uint64(math.Max(1, math.Round(float64(m)/float64(n)*math.Ln2)))
	words := (m + 63) / 64
	return &Bloom{bits: make([]uint64, words), m: words * 64, k: k, seed: maphash.MakeSeed()}
}

func (b *Bloom) hashes(key string) (uint64, uint64) {
	h1 := maphash.String(b.seed, key)
	// Second hash: remix h1 (splitmix64 finaliser) and force odd so strides cover m.
	h2 := h1 ^ (h1 >> 33)
	h2 *= 0xff51afd7ed558ccd
	h2 ^= h2 >> 33
	return h1, h2 | 1
}

// Add inserts key.
func (b *Bloom) Add(key string) {
	h1, h2 := b.hashes(key)
	for i := uint64(0); i < b.k; i++ {
		idx := (h1 + i*h2) % b.m
		b.bits[idx>>6] |= 1 << (idx & 63)
	}
}

// Test reports whether key may have been added (false positives possible,
// false negatives impossible).
func (b *Bloom) Test(key string) bool {
	h1, h2 := b.hashes(key)
	for i := uint64(0); i < b.k; i++ {
		idx := (h1 + i*h2) % b.m
		if b.bits[idx>>6]&(1<<(idx&63)) == 0 {
			return false
		}
	}
	return true
}

// TestAndAdd returns true if key was (probably) already present, and adds it.
func (b *Bloom) TestAndAdd(key string) bool {
	h1, h2 := b.hashes(key)
	present := true
	for i := uint64(0); i < b.k; i++ {
		idx := (h1 + i*h2) % b.m
		w, bit := idx>>6, uint64(1)<<(idx&63)
		if b.bits[w]&bit == 0 {
			present = false
			b.bits[w] |= bit
		}
	}
	return present
}

// K and M expose sizing for metrics/tests.
func (b *Bloom) K() uint64 { return b.k }
func (b *Bloom) M() uint64 { return b.m }

func (b *Bloom) reset() {
	for i := range b.bits {
		b.bits[i] = 0
	}
}

// Dedup is a time-rotating pair of Bloom filters. Keys are remembered for at
// least one rotation period and at most two, bounding memory regardless of
// stream length (a plain Bloom filter would saturate). Safe for concurrent use.
type Dedup struct {
	mu       sync.Mutex
	cur, old *Bloom
	period   time.Duration
	rotated  time.Time
	now      func() time.Time
}

// NewDedup: n = expected unique keys per period.
func NewDedup(n int, p float64, period time.Duration) *Dedup {
	return &Dedup{cur: NewBloom(n, p), old: NewBloom(n, p), period: period, rotated: time.Now(), now: time.Now}
}

// Seen returns true if key is a (probable) duplicate within the window.
func (d *Dedup) Seen(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.now(); t.Sub(d.rotated) >= d.period {
		if t.Sub(d.rotated) >= 2*d.period { // idle for 2+ periods: everything expired
			d.old.reset()
		} else {
			d.old, d.cur = d.cur, d.old
		}
		d.cur.reset()
		d.rotated = t
	}
	if d.old.Test(key) {
		d.cur.Add(key)
		return true
	}
	return d.cur.TestAndAdd(key)
}
