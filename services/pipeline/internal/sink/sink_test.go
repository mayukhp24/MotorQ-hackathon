package sink

import (
	"context"
	"errors"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/twmb/franz-go/pkg/kgo"

	"fleetpulse/pipeline/internal/detect"
	"fleetpulse/pipeline/internal/platform"
)

func recs(t *testing.T) []*kgo.Record {
	a, _ := json.Marshal(detect.Alert{AlertID: "6f1c1f0e-8d4b-4c38-9e59-5b1f4b8b2a10", TenantID: "t", VIN: "1HGCM82633A004352",
		Type: detect.AlertOverheat, Severity: detect.SevCritical, TsMs: 1_790_000_000_000, DetectedMs: 1_790_000_000_500,
		Details: map[string]any{"coolant_c": 115.2}})
	tr, _ := json.Marshal(detect.Trip{TripID: "7f1c1f0e-8d4b-4c38-9e59-5b1f4b8b2a10", TenantID: "t", VIN: "1HGCM82633A004352",
		StartMs: 1, EndMs: 2, StartLat: 12.97, StartLon: 77.59, EndLat: 13, EndLon: 77.6, DistanceKm: 4})
	return []*kgo.Record{
		{Topic: platform.TopicAlerts, Value: a},
		{Topic: platform.TopicTrips, Value: tr},
		{Topic: platform.TopicAlerts, Value: []byte("{garbage")},
		{Topic: platform.TopicTrips, Value: []byte("nope")},
		{Topic: "other", Value: []byte("{}")},
	}
}

func TestBuildBatch(t *testing.T) {
	b, na, nt := BuildBatch(recs(t))
	if na != 1 || nt != 1 || b.Len() != 2 {
		t.Fatalf("alerts=%d trips=%d queued=%d", na, nt, b.Len())
	}
	trip := b.QueuedQueries[1]
	if gh := trip.Arguments[13].(string); len(gh) != 6 {
		t.Fatalf("trip locations must be minimised to geohash-6, got %q", gh)
	}
}

type fakeResults struct {
	n, fail int
}

func (f *fakeResults) Exec() (pgconn.CommandTag, error) {
	f.n++
	if f.n == f.fail {
		return pgconn.CommandTag{}, errors.New("constraint")
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (f *fakeResults) Query() (pgx.Rows, error) { return nil, nil }
func (f *fakeResults) QueryRow() pgx.Row        { return nil }
func (f *fakeResults) Close() error             { return nil }

type fakeDB struct{ res *fakeResults }

func (d fakeDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return d.res }

func TestWrite(t *testing.T) {
	b, _, _ := BuildBatch(recs(t))
	if err := Write(context.Background(), fakeDB{&fakeResults{}}, b); err != nil {
		t.Fatal(err)
	}
	if err := Write(context.Background(), fakeDB{&fakeResults{fail: 2}}, b); err == nil {
		t.Fatal("expected failure to propagate (offsets must not be committed)")
	}
	if err := Write(context.Background(), fakeDB{&fakeResults{}}, &pgx.Batch{}); err != nil {
		t.Fatal(err)
	}
}
