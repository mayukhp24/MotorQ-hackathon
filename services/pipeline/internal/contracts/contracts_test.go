// Package contracts verifies the Kafka event contracts in /contracts from
// both sides: producers (Go structs, as actually serialised) must validate
// against the JSON Schemas, and the ClickHouse consumer DDL must map every
// field it needs. Breaking either side fails CI before deployment.
package contracts

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gojson "github.com/goccy/go-json"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/detect"
	"fleetpulse/pipeline/internal/oem"
	"fleetpulse/pipeline/internal/processor"
	"fleetpulse/pipeline/internal/sim"
)

const repo = "../../../../"

func schema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	s, err := c.Compile(filepath.Join(repo, "contracts", name))
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, raw []byte) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err != nil {
		t.Fatalf("contract violation: %v\npayload: %s", err, raw)
	}
}

type captureOut struct{ enriched, alerts, trips, dlq [][]byte }

func (c *captureOut) Enriched(_ string, v []byte)   { c.enriched = append(c.enriched, v) }
func (c *captureOut) Alert(_ string, v []byte)      { c.alerts = append(c.alerts, v) }
func (c *captureOut) Trip(_ string, v []byte)       { c.trips = append(c.trips, v) }
func (c *captureOut) DeadLetter(_ string, v []byte) { c.dlq = append(c.dlq, v) }

type nopLive struct{}

func (nopLive) WriteVehicles(context.Context, []processor.LiveRecord) error { return nil }
func (nopLive) PublishAlert(context.Context, string, []byte) error          { return nil }
func (nopLive) WriteSnapshot(context.Context, string, string, []byte) error { return nil }

type metaAll struct{ w *sim.World }

func (m metaAll) Get(vin string) (detect.VehicleMeta, bool) {
	v := m.w.ByVIN(vin)
	if v == nil {
		return detect.VehicleMeta{}, false
	}
	return detect.VehicleMeta{TenantID: v.Tenant.ID, FleetID: v.Fleet.ID, DriverID: v.Driver.ID, Powertrain: v.Powertrain}, true
}

// End to end through real code: simulator → OEM wire format → adapter →
// canonical (telemetry.v1) → processor → enriched/alerts/trips.
func TestProducersHonourContracts(t *testing.T) {
	w, err := sim.BuildWorld(7, 300)
	if err != nil {
		t.Fatal(err)
	}
	reg := oem.DefaultRegistry()
	sTele, sEnr, sAlert, sTrip := schema(t, "telemetry.v1.schema.json"), schema(t, "telemetry.enriched.v1.schema.json"),
		schema(t, "alert.v1.schema.json"), schema(t, "trip.v1.schema.json")
	out := &captureOut{}
	p := processor.New(processor.DefaultConfig(), detect.NewEngine(detect.DefaultThresholds()), metaAll{w}, out, nopLive{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	start := float64(time.Now().Add(-2 * time.Hour).Unix())
	checked := 0
	for i, v := range w.Vehicles {
		l := sim.NewLive(v, start, 1)
		var batch [][]byte
		for s := 0; s < 900; s++ { // 15 minutes at 1 Hz
			e := l.Step(start+float64(s), 1)
			if s == 100 { // force alert-producing conditions on a few samples
				e.DTC = []string{"P0217"}
				e.Events = []string{canonical.EvtCrash}
			}
			if s == 899 {
				e.Ignition, e.SpeedKmh = false, 0
			}
			enc := sim.EncoderFor(v.OEM)
			a, _ := reg.Get(v.OEM)
			evs, rej, err := a.Decode(enc.End(enc.Record(enc.Begin(nil), &e, true)), e.TsMs+30)
			if err != nil || len(rej) > 0 {
				t.Fatalf("decode: %v %v", err, rej)
			}
			raw, _ := gojson.Marshal(&evs[0])
			if s%97 == 0 {
				validate(t, sTele, raw)
				checked++
			}
			batch = append(batch, raw)
		}
		p.Process(context.Background(), int32(i%4), batch)
	}
	for i, b := range out.enriched {
		if i%211 == 0 {
			validate(t, sEnr, b)
		}
	}
	for _, b := range out.alerts {
		validate(t, sAlert, b)
	}
	for _, b := range out.trips {
		validate(t, sTrip, b)
	}
	if checked == 0 || len(out.alerts) == 0 || len(out.trips) == 0 || len(out.enriched) == 0 {
		t.Fatalf("coverage too thin: tele=%d enriched=%d alerts=%d trips=%d", checked, len(out.enriched), len(out.alerts), len(out.trips))
	}
	t.Logf("validated: telemetry=%d enriched=%d alerts=%d trips=%d", checked, len(out.enriched)/211+1, len(out.alerts), len(out.trips))
}

var colRe = regexp.MustCompile(`(?m)^\s*([a-z_]+)\s+[A-Z]`)

// Consumer side: the ClickHouse Kafka-engine tables must declare a column for
// every required field of the schema they consume (JSONEachRow maps by name).
func TestClickHouseConsumerCoversContract(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join(repo, "db", "clickhouse", "001_telemetry.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for table, file := range map[string]string{"telemetry_queue": "telemetry.enriched.v1.schema.json", "alerts_queue": "alert.v1.schema.json"} {
		block := tableBlock(t, string(ddl), table)
		cols := map[string]bool{}
		for _, m := range colRe.FindAllStringSubmatch(block, -1) {
			cols[m[1]] = true
		}
		for _, part := range strings.Split(block, ",") { // inline "a String, b Int64" lists
			if f := strings.Fields(strings.TrimSpace(part)); len(f) >= 2 {
				cols[strings.Trim(f[0], "(")] = true
			}
		}
		raw, _ := os.ReadFile(filepath.Join(repo, "contracts", file))
		var sc struct {
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(raw, &sc)
		needed := sc.Required
		if table == "alerts_queue" { // analytics copy keeps the fields it aggregates on
			needed = []string{"alert_id", "tenant_id", "fleet_id", "vin", "type", "severity", "ts_ms", "ingest_ms", "detected_ms"}
		}
		for _, f := range needed {
			if !cols[f] {
				t.Errorf("%s: no column for contract field %q", table, f)
			}
		}
	}
}

func tableBlock(t *testing.T, ddl, table string) string {
	i := strings.Index(ddl, "fleet."+table)
	if i < 0 {
		t.Fatalf("table %s not found", table)
	}
	j := strings.Index(ddl[i:], "ENGINE")
	return ddl[i : i+j]
}
