package processor

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"fleetpulse/pipeline/internal/canonical"
)

func TestRedisLiveStore(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	live := &RedisLive{R: rdb}
	ctx := context.Background()
	recs := []LiveRecord{
		{VIN: "1HGCM82633A004352", TenantID: "t1", FleetID: "f1", Status: "DRIVING", Powertrain: "ICE", Geohash: "tdr1vzc",
			Lat: 12.97, Lon: 77.59, Speed: 42.5, Odo: 1000, Coolant: canonical.F(91), BattV: canonical.F(13.9), TsMs: 1, Ignition: true},
		{VIN: "1M8GDM9AXKP042788", TenantID: "t1", Status: "CHARGING", Powertrain: "EV", Lat: 13.0, Lon: 77.6, Soc: canonical.F(55)},
	}
	if err := live.WriteVehicles(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("v:1HGCM82633A004352", "st"); got != "DRIVING" {
		t.Fatalf("status = %q", got)
	}
	if got := mr.HGet("v:1HGCM82633A004352", "cool"); got != "91.0" {
		t.Fatalf("coolant = %q", got)
	}
	if got := mr.HGet("v:1M8GDM9AXKP042788", "soc"); got != "55.0" {
		t.Fatalf("soc = %q", got)
	}
	res, err := rdb.GeoSearch(ctx, "geo:t1", &redis.GeoSearchQuery{Longitude: 77.59, Latitude: 12.97, Radius: 20, RadiusUnit: "km"}).Result()
	if err != nil || len(res) != 2 {
		t.Fatalf("geo members = %v err=%v", res, err)
	}
	if err := live.WriteSnapshot(ctx, "kpi:t1", "3", []byte(`{"online":1}`)); err != nil {
		t.Fatal(err)
	}
	if mr.HGet("kpi:t1", "3") != `{"online":1}` || mr.TTL("kpi:t1") <= 0 {
		t.Fatal("snapshot not stored with TTL")
	}
	sub := rdb.Subscribe(ctx, "alerts:t1")
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := live.PublishAlert(ctx, "t1", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	msg, err := sub.ReceiveMessage(ctx)
	if err != nil || msg.Payload != `{"a":1}` {
		t.Fatalf("pubsub: %v %v", msg, err)
	}
}

func TestPGMetaEmpty(t *testing.T) {
	m := &PGMeta{}
	if _, ok := m.Get("X"); ok {
		t.Fatal("empty registry must not resolve")
	}
}
