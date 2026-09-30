package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/oem"
)

type fakeSink struct {
	mu       sync.Mutex
	events   []canonical.Event
	rejects  []oem.Reject
	pressure float64
	err      error
	block    chan struct{}
}

func (f *fakeSink) Produce(ctx context.Context, e []canonical.Event, r []oem.Reject) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, e...)
	f.rejects = append(f.rejects, r...)
	return nil
}
func (f *fakeSink) Pressure() float64 { return f.pressure }

var nowT = time.UnixMilli(1790000000123)

func gw(s Sink) *Gateway {
	g := NewGateway(oem.DefaultRegistry(), s, slog.New(slog.NewTextHandler(io.Discard, nil)), 2, "aurora:secret-a, pinnacle:secret-p,bad")
	g.Now = func() time.Time { return nowT }
	return g
}

const good = `{"records":[
 {"vehicle":{"vin":"1HGCM82633A004352"},"timestamp":1790000000000,"sequence":1,"position":{"latitude":12.9,"longitude":77.6},
  "motion":{"speedKph":30},"powertrain":{"odometerKm":10,"engineOn":true}},
 {"vehicle":{"vin":"1HGCM82643A004352"},"timestamp":1790000000000,"sequence":2,"position":{"latitude":12.9,"longitude":77.6},
  "motion":{"speedKph":30},"powertrain":{"odometerKm":10,"engineOn":true}},
 {"vehicle":{"vin":"1HGCM82633A004352"},"timestamp":1790000000000,"sequence":3,"position":{"latitude":12.9,"longitude":77.6},
  "motion":{"speedKph":30}}
]}`

func post(t *testing.T, h http.Handler, path, key string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTP_AcceptsValidatesAndDLQs(t *testing.T) {
	s := &fakeSink{}
	h := gw(s).HTTPHandler()
	rec := post(t, h, "/v1/ingest/aurora", "secret-a", strings.NewReader(good), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"accepted":1`) || !strings.Contains(rec.Body.String(), `"rejected":2`) {
		t.Fatalf("body=%s", rec.Body)
	}
	if len(s.events) != 1 || len(s.rejects) != 2 {
		t.Fatalf("events=%d rejects=%d", len(s.events), len(s.rejects))
	}
	if s.events[0].IngestMs != nowT.UnixMilli() {
		t.Fatal("ingest timestamp not stamped")
	}
	reasons := s.rejects[0].Reason + "|" + s.rejects[1].Reason
	if !strings.Contains(reasons, "check digit") || !strings.Contains(reasons, "missing odo_km") {
		t.Fatalf("reasons=%s", reasons)
	}
}

func TestHTTP_Auth(t *testing.T) {
	h := gw(&fakeSink{}).HTTPHandler()
	for _, key := range []string{"", "wrong", "secret-p"} {
		if rec := post(t, h, "/v1/ingest/aurora", key, strings.NewReader(good), nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: code %d", key, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest/aurora", strings.NewReader(good))
	req.Header.Set("Authorization", "Bearer secret-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("bearer auth failed: %d", rec.Code)
	}
}

func TestHTTP_Gzip(t *testing.T) {
	s := &fakeSink{}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(good))
	_ = zw.Close()
	rec := post(t, gw(s).HTTPHandler(), "/v1/ingest/aurora", "secret-a", &buf, map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != http.StatusAccepted || len(s.events) != 1 {
		t.Fatalf("code=%d events=%d", rec.Code, len(s.events))
	}
	rec = post(t, gw(s).HTTPHandler(), "/v1/ingest/aurora", "secret-a", strings.NewReader("notgzip"), map[string]string{"Content-Encoding": "gzip"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHTTP_Unparseable(t *testing.T) {
	s := &fakeSink{}
	rec := post(t, gw(s).HTTPHandler(), "/v1/ingest/aurora", "secret-a", strings.NewReader("{oops"), nil)
	if rec.Code != http.StatusBadRequest || len(s.rejects) != 1 {
		t.Fatalf("code=%d rejects=%d", rec.Code, len(s.rejects))
	}
}

func TestHTTP_BackPressure(t *testing.T) {
	s := &fakeSink{pressure: 0.95}
	rec := post(t, gw(s).HTTPHandler(), "/v1/ingest/aurora", "secret-a", strings.NewReader(good), nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", rec.Code)
	}
	// Saturate in-flight slots (max 2) with blocked requests.
	s2 := &fakeSink{block: make(chan struct{})}
	g := gw(s2)
	h := g.HTTPHandler()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); post(t, h, "/v1/ingest/aurora", "secret-a", strings.NewReader(good), nil) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(g.sem) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rec := post(t, h, "/v1/ingest/aurora", "secret-a", strings.NewReader(good), nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when saturated, got %d", rec.Code)
	}
	close(s2.block)
	wg.Wait()
}

func TestHTTP_SinkErrorIs503(t *testing.T) {
	s := &fakeSink{err: errors.New("broker down")}
	rec := post(t, gw(s).HTTPHandler(), "/v1/ingest/aurora", "secret-a", strings.NewReader(good), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHandle_UnknownOEM(t *testing.T) {
	g := gw(&fakeSink{})
	g.keys["ghost"] = g.keys["aurora"]
	rec := post(t, g.HTTPHandler(), "/v1/ingest/ghost", "secret-a", strings.NewReader(good), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAdaptersEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/adapters", nil)
	rec := httptest.NewRecorder()
	gw(&fakeSink{}).HTTPHandler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "stellar@v1") {
		t.Fatalf("body=%s", rec.Body)
	}
}

func TestOEMFromTopic(t *testing.T) {
	if oemFromTopic("fleetpulse/oem/aurora/telemetry") != "aurora" || oemFromTopic("x/y") != "" {
		t.Fatal("topic parsing")
	}
}
