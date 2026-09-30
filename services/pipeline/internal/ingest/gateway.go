// Package ingest implements the ingestion gateway: authenticated HTTPS and
// MQTT intake of OEM batches, schema validation, normalisation and durable
// hand-off to Kafka, with explicit back-pressure.
package ingest

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/oem"
)

// Sink is the outbound port (Kafka in production, a fake in tests).
type Sink interface {
	// Produce enqueues events and rejects. It may block when the producer
	// buffer is full (back-pressure) and must honour ctx.
	Produce(ctx context.Context, events []canonical.Event, rejects []oem.Reject) error
	// Pressure returns producer buffer utilisation in [0,1].
	Pressure() float64
}

var (
	mBatches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_batches_total", Help: "OEM batches received"}, []string{"oem", "transport", "result"})
	mEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_events_total", Help: "Records by outcome"}, []string{"oem", "result"})
	mLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ingest_batch_seconds", Help: "Time to decode, validate and enqueue a batch",
		Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14)}, []string{"transport"})
	mBackpressure = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_backpressure_total", Help: "Requests shed due to back-pressure"}, []string{"transport"})
	mInflight = promauto.NewGauge(prometheus.GaugeOpts{Name: "ingest_inflight_batches", Help: "Batches being processed"})
)

// Gateway is the application service shared by all transports.
type Gateway struct {
	Registry  *oem.Registry
	Sink      Sink
	Log       *slog.Logger
	Now       func() time.Time
	sem       chan struct{}
	keys      map[string][32]byte // oem -> sha256(api key)
	MaxBody   int64
	HighWater float64
}

// NewGateway. apiKeys format: "aurora:key1,pinnacle:key2".
func NewGateway(reg *oem.Registry, sink Sink, log *slog.Logger, maxInflight int, apiKeys string) *Gateway {
	g := &Gateway{Registry: reg, Sink: sink, Log: log, Now: time.Now, sem: make(chan struct{}, maxInflight),
		keys: map[string][32]byte{}, MaxBody: 8 << 20, HighWater: 0.9}
	for _, kv := range strings.Split(apiKeys, ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), ":", 2)
		if len(parts) == 2 && parts[1] != "" {
			g.keys[parts[0]] = sha256.Sum256([]byte(parts[1]))
		}
	}
	return g
}

var (
	ErrBusy         = errors.New("gateway saturated")
	ErrUnauthorized = errors.New("unauthorized")
)

// Result summarises one batch.
type Result struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

// acquire applies back-pressure: bounded in-flight batches and a producer
// high-water mark. Callers shed load (HTTP 429 / MQTT redelivery) instead of
// buffering unboundedly.
func (g *Gateway) acquire(ctx context.Context, wait time.Duration) error {
	if g.Sink.Pressure() >= g.HighWater {
		return ErrBusy
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case g.sem <- struct{}{}:
		mInflight.Inc()
		return nil
	case <-t.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gateway) release() { <-g.sem; mInflight.Dec() }

// Authorize checks a connector API key in constant time. In production the
// same identity comes from the mTLS client certificate (see docs/security).
func (g *Gateway) Authorize(oemCode, key string) bool {
	want, ok := g.keys[oemCode]
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// Handle normalises one OEM batch and enqueues it.
func (g *Gateway) Handle(ctx context.Context, oemCode, transport string, payload []byte) (Result, error) {
	start := g.Now()
	defer func() { mLatency.WithLabelValues(transport).Observe(time.Since(start).Seconds()) }()
	a, err := g.Registry.Get(oemCode)
	if err != nil {
		mBatches.WithLabelValues("unknown", transport, "unknown_oem").Inc()
		return Result{}, err
	}
	events, rejects, err := a.Decode(payload, start.UnixMilli())
	if err != nil {
		mBatches.WithLabelValues(oemCode, transport, "unparseable").Inc()
		rejects = []oem.Reject{{OEM: oemCode, Reason: err.Error(), Raw: truncate(payload, 4096)}}
		_ = g.Sink.Produce(ctx, nil, rejects)
		return Result{Rejected: 1}, err
	}
	nowMs := start.UnixMilli()
	valid := events[:0]
	for i := range events {
		if verr := events[i].Validate(nowMs); verr != nil {
			raw, _ := json.Marshal(events[i])
			rejects = append(rejects, oem.Reject{OEM: oemCode, Reason: verr.Error(), Raw: string(raw)})
			continue
		}
		valid = append(valid, events[i])
	}
	if err := g.Sink.Produce(ctx, valid, rejects); err != nil {
		mBatches.WithLabelValues(oemCode, transport, "sink_error").Inc()
		return Result{}, err
	}
	mBatches.WithLabelValues(oemCode, transport, "ok").Inc()
	mEvents.WithLabelValues(oemCode, "accepted").Add(float64(len(valid)))
	mEvents.WithLabelValues(oemCode, "rejected").Add(float64(len(rejects)))
	return Result{Accepted: len(valid), Rejected: len(rejects)}, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

// HTTPHandler exposes POST /v1/ingest/{oem}.
func (g *Gateway) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest/{oem}", func(w http.ResponseWriter, r *http.Request) {
		oemCode := r.PathValue("oem")
		key := r.Header.Get("X-Api-Key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if !g.Authorize(oemCode, key) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid connector credentials"})
			return
		}
		if err := g.acquire(r.Context(), 50*time.Millisecond); err != nil {
			mBackpressure.WithLabelValues("http").Inc()
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "back-pressure: retry later"})
			return
		}
		defer g.release()
		var body io.Reader = http.MaxBytesReader(w, r.Body, g.MaxBody)
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			zr, err := gzip.NewReader(body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad gzip body"})
				return
			}
			defer zr.Close()
			body = io.LimitReader(zr, 4*g.MaxBody)
		}
		payload, err := io.ReadAll(body)
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
			return
		}
		res, err := g.Handle(r.Context(), oemCode, "http", payload)
		switch {
		case errors.Is(err, oem.ErrUnknownOEM):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case err != nil && res.Rejected > 0:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "rejected": res.Rejected})
		case err != nil:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		default:
			writeJSON(w, http.StatusAccepted, res)
		}
	})
	mux.HandleFunc("GET /v1/adapters", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"adapters": g.Registry.List()})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
