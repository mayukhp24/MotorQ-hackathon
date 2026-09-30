// Package oem normalises OEM-specific telemetry payloads into the canonical
// event (Adapter pattern). Each OEM cloud encodes events differently; new
// OEMs are onboarded by dropping a mapping spec into the adapters directory
// and hot-reloading, with no code change or downtime.
package oem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"fleetpulse/pipeline/internal/canonical"
)

// Reject is a record that could not be normalised. It is routed to the DLQ
// with its raw bytes so it can be replayed after an adapter fix.
type Reject struct {
	OEM    string `json:"oem"`
	Reason string `json:"reason"`
	Raw    string `json:"raw"`
}

// Adapter decodes one OEM batch payload.
type Adapter interface {
	OEM() string
	Version() int
	// Decode returns canonical events and per-record rejects. err is set only
	// when the whole payload is unreadable.
	Decode(payload []byte, recvMs int64) ([]canonical.Event, []Reject, error)
}

var ErrUnknownOEM = errors.New("unknown oem")

// Registry is a copy-on-write map of adapters; lookups are lock-free so the
// hot path never contends with reloads.
type Registry struct {
	mu       sync.Mutex // serialises writers
	adapters atomic.Pointer[map[string]Adapter]
}

func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{}
	m := map[string]Adapter{}
	for _, a := range adapters {
		m[a.OEM()] = a
	}
	r.adapters.Store(&m)
	return r
}

// Get returns the adapter for an OEM code.
func (r *Registry) Get(oem string) (Adapter, error) {
	m := *r.adapters.Load()
	a, ok := m[oem]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownOEM, oem)
	}
	return a, nil
}

// Put registers or replaces an adapter atomically.
func (r *Registry) Put(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := *r.adapters.Load()
	m := make(map[string]Adapter, len(old)+1)
	for k, v := range old {
		m[k] = v
	}
	m[a.OEM()] = a
	r.adapters.Store(&m)
}

// List returns "oem@version" entries, sorted.
func (r *Registry) List() []string {
	m := *r.adapters.Load()
	out := make([]string, 0, len(m))
	for k, a := range m {
		out = append(out, fmt.Sprintf("%s@v%d", k, a.Version()))
	}
	sort.Strings(out)
	return out
}

// LoadSpecDir loads every *.json mapping spec in dir into the registry.
// Invalid specs are reported and skipped; valid ones replace old versions.
func (r *Registry) LoadSpecDir(dir string) (loaded []string, errs []error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, []error{err}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a, err := NewMappingAdapter(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(f), err))
			continue
		}
		r.Put(a)
		loaded = append(loaded, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	return loaded, errs
}

// DefaultRegistry returns the built-in adapters: two declarative specs and
// one code adapter for the signal-list format.
func DefaultRegistry() *Registry {
	r := NewRegistry(NewStellarAdapter())
	for _, spec := range [][]byte{AuroraSpec, PinnacleSpec} {
		a, err := NewMappingAdapter(spec)
		if err != nil {
			panic(err) // built-in specs are covered by tests
		}
		r.Put(a)
	}
	return r
}
