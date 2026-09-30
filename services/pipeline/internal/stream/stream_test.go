package stream

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestBloom_NoFalseNegatives(t *testing.T) {
	b := NewBloom(10000, 0.01)
	for i := 0; i < 10000; i++ {
		b.Add(fmt.Sprint("k", i))
	}
	for i := 0; i < 10000; i++ {
		if !b.Test(fmt.Sprint("k", i)) {
			t.Fatalf("false negative for k%d", i)
		}
	}
}

func TestBloom_FalsePositiveRateWithinBound(t *testing.T) {
	const n, p = 50000, 0.01
	b := NewBloom(n, p)
	for i := 0; i < n; i++ {
		b.Add(fmt.Sprint("in", i))
	}
	fp := 0
	const probes = 100000
	for i := 0; i < probes; i++ {
		if b.Test(fmt.Sprint("out", i)) {
			fp++
		}
	}
	rate := float64(fp) / probes
	if rate > 2*p {
		t.Fatalf("fp rate %.4f exceeds 2x target %.4f (m=%d k=%d)", rate, p, b.M(), b.K())
	}
}

func TestBloom_Sizing(t *testing.T) {
	b := NewBloom(1000, 0.001)
	// m/n ≈ 14.4 bits per item, k ≈ 10 for p=0.1%.
	if b.K() < 9 || b.K() > 11 {
		t.Fatalf("k=%d", b.K())
	}
	b2 := NewBloom(0, 5) // invalid inputs fall back to defaults
	if b2.M() == 0 {
		t.Fatal("expected non-empty filter")
	}
}

func TestBloom_TestAndAdd(t *testing.T) {
	b := NewBloom(100, 0.01)
	if b.TestAndAdd("x") {
		t.Fatal("first insert reported present")
	}
	if !b.TestAndAdd("x") {
		t.Fatal("second insert not reported present")
	}
}

func TestDedup_WindowRotation(t *testing.T) {
	clock := time.Unix(0, 0)
	d := NewDedup(1000, 0.001, time.Minute)
	d.now = func() time.Time { return clock }
	d.rotated = clock
	if d.Seen("a") {
		t.Fatal("new key flagged")
	}
	if !d.Seen("a") {
		t.Fatal("duplicate missed")
	}
	clock = clock.Add(61 * time.Second) // rotation 1: "a" moves to old, still remembered
	if !d.Seen("a") {
		t.Fatal("duplicate missed after one rotation")
	}
	clock = clock.Add(61 * time.Second) // "a" was re-added to cur, still remembered
	if !d.Seen("a") {
		t.Fatal("recently seen key forgotten")
	}
	if d.Seen("b") {
		t.Fatal("new key flagged")
	}
	clock = clock.Add(122 * time.Second) // two periods without seeing b: forgotten
	if d.Seen("b") {
		t.Fatal("key should expire after two idle rotations")
	}
}

func TestCountMin_NeverUndercounts(t *testing.T) {
	c := NewCountMin(0.001, 0.01)
	truth := map[string]uint32{}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 100000; i++ {
		k := fmt.Sprint("k", int(math.Abs(r.NormFloat64()*50)))
		truth[k]++
		c.Add(k, 1)
	}
	var maxErr uint32
	for k, v := range truth {
		est := c.Estimate(k)
		if est < v {
			t.Fatalf("undercount for %s: %d < %d", k, est, v)
		}
		if est-v > maxErr {
			maxErr = est - v
		}
	}
	bound := uint32(0.001 * float64(c.Total()) * 2)
	if maxErr > bound {
		t.Fatalf("max over-count %d exceeds bound %d", maxErr, bound)
	}
}

func TestTopK_FindsHeavyHitters(t *testing.T) {
	tk := NewTopK(3, 0.001, 0.01)
	stream := map[string]int{"P0301": 500, "P0217": 300, "P0562": 200, "P0420": 20, "U0100": 10}
	for rep := 0; rep < 10; rep++ { // interleave
		for k, n := range stream {
			for i := 0; i < n/10; i++ {
				tk.Add(k)
			}
		}
	}
	for i := 0; i < 50; i++ {
		tk.Add(fmt.Sprint("noise", i))
	}
	items := tk.Items()
	if len(items) != 3 {
		t.Fatalf("len=%d", len(items))
	}
	want := []string{"P0301", "P0217", "P0562"}
	for i, w := range want {
		if items[i].Key != w {
			t.Fatalf("rank %d = %s want %s (%v)", i, items[i].Key, w, items)
		}
	}
	if tk.Total() != 1080 {
		t.Fatalf("total=%d", tk.Total())
	}
}

func TestGeohash_KnownValues(t *testing.T) {
	// Reference values from the canonical geohash algorithm.
	cases := []struct {
		lat, lon float64
		p        int
		want     string
	}{
		{57.64911, 10.40744, 11, "u4pruydqqvj"},
		{42.605, -5.603, 5, "ezs42"},
		{0, 0, 1, "s"},
		{-90, -180, 3, "000"},
	}
	for _, c := range cases {
		if got := Geohash(c.lat, c.lon, c.p); got != c.want {
			t.Errorf("Geohash(%v,%v,%d)=%s want %s", c.lat, c.lon, c.p, got, c.want)
		}
	}
	if Geohash(1, 1, 0) != "" {
		t.Fatal("precision 0 should be empty")
	}
}

func TestGeohash_RoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 1000; i++ {
		lat, lon := r.Float64()*180-90, r.Float64()*360-180
		h := Geohash(lat, lon, 9)
		clat, clon := GeohashCenter(h)
		if math.Abs(clat-lat) > 0.0001 || math.Abs(clon-lon) > 0.0001 {
			t.Fatalf("round trip too lossy: %v,%v -> %s -> %v,%v", lat, lon, h, clat, clon)
		}
		if Geohash(lat, lon, 5) != h[:5] {
			t.Fatal("prefix property violated")
		}
	}
	if la, lo := GeohashCenter("a!"); la != 0 || lo != 0 {
		t.Fatal("invalid geohash should decode to 0,0")
	}
}

func BenchmarkDedup(b *testing.B) {
	d := NewDedup(1_000_000, 0.001, time.Minute)
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprint("aurora:1HGCM82633A004352:", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Seen(keys[i&4095])
	}
}

func BenchmarkGeohash(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Geohash(12.9716, 77.5946, 7)
	}
}
