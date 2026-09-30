package stream

import (
	"container/heap"
	"hash/maphash"
	"math"
	"sort"
)

// CountMin is a Count-Min Sketch: d rows of w counters. Estimates never
// under-count; over-count is bounded by eps*N with probability 1-delta where
// w = ceil(e/eps), d = ceil(ln(1/delta)).
// Add/Estimate: O(d) time; space O(w*d).
type CountMin struct {
	w, d  uint64
	table [][]uint32
	seeds []maphash.Seed
	total uint64
}

func NewCountMin(eps, delta float64) *CountMin {
	w := uint64(math.Ceil(math.E / eps))
	d := uint64(math.Ceil(math.Log(1 / delta)))
	t := make([][]uint32, d)
	seeds := make([]maphash.Seed, d)
	for i := range t {
		t[i] = make([]uint32, w)
		seeds[i] = maphash.MakeSeed()
	}
	return &CountMin{w: w, d: d, table: t, seeds: seeds}
}

// Add increments key by c and returns the new estimate.
func (c *CountMin) Add(key string, n uint32) uint32 {
	c.total += uint64(n)
	est := uint32(math.MaxUint32)
	for i := uint64(0); i < c.d; i++ {
		j := maphash.String(c.seeds[i], key) % c.w
		c.table[i][j] += n
		if c.table[i][j] < est {
			est = c.table[i][j]
		}
	}
	return est
}

// Estimate returns the (over-)estimated count of key.
func (c *CountMin) Estimate(key string) uint32 {
	est := uint32(math.MaxUint32)
	for i := uint64(0); i < c.d; i++ {
		j := maphash.String(c.seeds[i], key) % c.w
		if c.table[i][j] < est {
			est = c.table[i][j]
		}
	}
	return est
}

func (c *CountMin) Total() uint64 { return c.total }

// Item is a key with its estimated count.
type Item struct {
	Key   string `json:"key"`
	Count uint32 `json:"count"`
}

type minHeap []Item

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].Count < h[j].Count }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(Item)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// TopK tracks the K heaviest keys of a stream with a Count-Min Sketch plus a
// size-K min-heap. Update: O(d + K) (linear scan to find an existing key; K is
// small, e.g. 10). Memory: O(w*d + K), independent of distinct-key count.
type TopK struct {
	k    int
	cms  *CountMin
	heap minHeap
}

func NewTopK(k int, eps, delta float64) *TopK {
	return &TopK{k: k, cms: NewCountMin(eps, delta)}
}

func (t *TopK) Add(key string) {
	est := t.cms.Add(key, 1)
	for i := range t.heap {
		if t.heap[i].Key == key {
			t.heap[i].Count = est
			heap.Fix(&t.heap, i)
			return
		}
	}
	if len(t.heap) < t.k {
		heap.Push(&t.heap, Item{key, est})
		return
	}
	if est > t.heap[0].Count {
		t.heap[0] = Item{key, est}
		heap.Fix(&t.heap, 0)
	}
}

// Items returns the current top-K, heaviest first.
func (t *TopK) Items() []Item {
	out := make([]Item, len(t.heap))
	copy(out, t.heap)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Key < out[j].Key
		}
		return out[i].Count > out[j].Count
	})
	return out
}

func (t *TopK) Total() uint64 { return t.cms.Total() }
