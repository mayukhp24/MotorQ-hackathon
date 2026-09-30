// Package seed loads the synthetic world into PostgreSQL (master data,
// maintenance history, trips, alerts) and ClickHouse (daily feature history)
// so the platform is fully populated on first start.
package seed

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"fleetpulse/pipeline/internal/dtc"
	"fleetpulse/pipeline/internal/migrate"
	"fleetpulse/pipeline/internal/sim"
	"fleetpulse/pipeline/internal/stream"
)

type Options struct {
	HistoryDays  int
	TripDays     int
	AlertDays    int
	DemoPassword string
	PIIKey       string
	Now          time.Time
	Force        bool
}

func uid(s string) pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.MustParse(s), Valid: true}
}

func day(t time.Time) int { return int(t.Unix() / 86400) }

// DemoUser describes a seeded login.
type DemoUser struct {
	Email, Name, Role string
	TenantID          string // empty = platform operator
}

// DemoUsers returns the seeded accounts (documented in the README).
func DemoUsers(w *sim.World) []DemoUser {
	out := []DemoUser{{Email: "ops@fleetpulse.dev", Name: "Platform Operator", Role: "platform_admin"}}
	for _, t := range w.Tenants {
		short := strings.Split(t.Slug, "-")[0]
		out = append(out,
			DemoUser{"admin@" + short + ".demo", t.Name + " Admin", "fleet_admin", t.ID},
			DemoUser{"maint@" + short + ".demo", t.Name + " Maintenance Lead", "maintenance_manager", t.ID},
			DemoUser{"analyst@" + short + ".demo", t.Name + " Analyst", "analyst", t.ID},
			DemoUser{"viewer@" + short + ".demo", t.Name + " Viewer", "viewer", t.ID},
		)
	}
	return out
}

// Postgres seeds the relational store. Idempotent: skips when already
// seeded with the same fleet size unless Force is set.
func Postgres(ctx context.Context, pool *pgxpool.Pool, w *sim.World, o Options, log *slog.Logger) error {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vehicle`).Scan(&n); err != nil {
		return err
	}
	if n == len(w.Vehicles) && !o.Force {
		log.Info("postgres already seeded; skipping", "vehicles", n)
		return nil
	}
	start := time.Now()
	if n > 0 {
		log.Info("re-seeding postgres", "existing_vehicles", n)
		if _, err := pool.Exec(ctx, `TRUNCATE trip, alert, risk_score, service_event, work_order, vehicle_assignment,
			vehicle, driver, fleet, depot, user_role, app_user, subscription, agent_action, erasure_request, tenant CASCADE`); err != nil {
			return err
		}
	}
	if err := reference(ctx, pool); err != nil {
		return fmt.Errorf("reference data: %w", err)
	}
	if err := tenants(ctx, pool, w, o); err != nil {
		return fmt.Errorf("tenants: %w", err)
	}
	if err := drivers(ctx, pool, w, o); err != nil {
		return fmt.Errorf("drivers: %w", err)
	}
	if err := vehicles(ctx, pool, w, o); err != nil {
		return fmt.Errorf("vehicles: %w", err)
	}
	log.Info("seeded master data", "vehicles", len(w.Vehicles), "drivers", len(w.Drivers), "elapsed", time.Since(start).Round(time.Millisecond))
	if err := history(ctx, pool, w, o, log); err != nil {
		return fmt.Errorf("history: %w", err)
	}
	// Populate read models and set visibility maps so index-only scans work.
	for _, stmt := range []string{`SELECT refresh_vehicle_risk()`, `SELECT refresh_read_models()`, `VACUUM (ANALYZE)`} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	log.Info("postgres seed complete", "elapsed", time.Since(start).Round(time.Second))
	return nil
}

func reference(ctx context.Context, pool *pgxpool.Pool) error {
	b := &pgx.Batch{}
	for _, p := range []struct {
		code, name string
		price      float64
		rate       int
	}{{"starter", "Starter", 4, 600}, {"business", "Business", 7, 3000}, {"enterprise", "Enterprise", 11, 12000}} {
		b.Queue(`INSERT INTO plan VALUES ($1,$2,$3,$4,'{}') ON CONFLICT DO NOTHING`, p.code, p.name, p.price, p.rate)
	}
	for code, desc := range map[string]string{
		"platform_admin":      "Operates the platform across tenants",
		"fleet_admin":         "Full control of one tenant, including users, audit and privacy requests",
		"maintenance_manager": "Manages alerts, predictions and work orders; sees precise locations",
		"analyst":             "Read-only analytics with masked locations and pseudonymised drivers",
		"viewer":              "Dashboard read-only access",
	} {
		b.Queue(`INSERT INTO role VALUES ($1,$2) ON CONFLICT DO NOTHING`, code, desc)
	}
	for _, o := range sim.OEMs {
		b.Queue(`INSERT INTO oem VALUES ($1,$2,$3,1) ON CONFLICT DO NOTHING`, o.Code, o.Name, o.WMI)
	}
	for _, m := range sim.Models {
		id := sim.DeterministicID("model", m.OEM+"/"+m.Name)
		var batt, tank *float64
		if m.BatteryKwh > 0 {
			batt = &m.BatteryKwh
		}
		if m.TankL > 0 {
			tank = &m.TankL
		}
		b.Queue(`INSERT INTO vehicle_model VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
			uid(id), m.OEM, m.Name, m.Powertrain, m.BodyType, batt, tank)
	}
	for _, c := range dtc.Catalogue {
		b.Queue(`INSERT INTO dtc_code VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, c.Code, c.Description, string(c.Severity), string(c.Component))
	}
	return pool.SendBatch(ctx, b).Close()
}

func tenants(ctx context.Context, pool *pgxpool.Pool, w *sim.World, o Options) error {
	counts := map[string]int{}
	for _, v := range w.Vehicles {
		counts[v.Tenant.ID]++
	}
	b := &pgx.Batch{}
	for _, t := range w.Tenants {
		b.Queue(`INSERT INTO tenant (tenant_id, slug, name) VALUES ($1,$2,$3)`, uid(t.ID), t.Slug, t.Name)
		b.Queue(`INSERT INTO subscription (tenant_id, plan_code, status, vehicle_quota, starts_on) VALUES ($1,$2,'ACTIVE',$3,$4)`,
			uid(t.ID), t.Plan, int32(counts[t.ID]*12/10+100), o.Now.AddDate(0, -6, 0))
	}
	for _, d := range w.Depots {
		b.Queue(`INSERT INTO depot VALUES ($1,$2,$3,$4,$5,$6)`, uid(d.ID), uid(d.TenantID), d.Name, d.City.Name, d.Lat, d.Lon)
	}
	for _, f := range w.Fleets {
		b.Queue(`INSERT INTO fleet VALUES ($1,$2,$3,$4)`, uid(f.ID), uid(f.TenantID), uid(f.DepotID), f.Name)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(o.DemoPassword), 10)
	if err != nil {
		return err
	}
	for _, u := range DemoUsers(w) {
		id := sim.DeterministicID("user", u.Email)
		var tenant pgtype.UUID
		if u.TenantID != "" {
			tenant = uid(u.TenantID)
		}
		b.Queue(`INSERT INTO app_user (user_id, tenant_id, email, full_name, password_hash) VALUES ($1,$2,$3,$4,$5)`,
			uid(id), tenant, u.Email, u.Name, string(hash))
		b.Queue(`INSERT INTO user_role VALUES ($1,$2)`, uid(id), u.Role)
	}
	return pool.SendBatch(ctx, b).Close()
}

// drivers: PII is encrypted in the database with pgcrypto (AES-256) using a
// key supplied at runtime; only a SHA-256 of the licence is kept in clear
// for de-duplication lookups.
func drivers(ctx context.Context, pool *pgxpool.Pool, w *sim.World, o Options) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE driver_stage (driver_id uuid, tenant_id uuid, full_name text,
		license_no text, phone text) ON COMMIT DROP`); err != nil {
		return err
	}
	i := 0
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"driver_stage"},
		[]string{"driver_id", "tenant_id", "full_name", "license_no", "phone"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if i >= len(w.Drivers) {
				return nil, nil
			}
			d := w.Drivers[i]
			i++
			return []any{uid(d.ID), uid(d.TenantID), d.Name, d.LicenseNo, d.Phone}, nil
		})); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO driver (driver_id, tenant_id, full_name, license_hash, license_no_enc, phone_enc)
		SELECT driver_id, tenant_id, full_name, encode(digest(license_no, 'sha256'), 'hex'),
		       pgp_sym_encrypt(license_no, $1, 'cipher-algo=aes256, s2k-mode=1'),
		       pgp_sym_encrypt(phone, $1, 'cipher-algo=aes256, s2k-mode=1')
		FROM driver_stage`, o.PIIKey); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func vehicles(ctx context.Context, pool *pgxpool.Pool, w *sim.World, o Options) error {
	i := 0
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"vehicle"},
		[]string{"vin", "tenant_id", "fleet_id", "model_id", "model_year", "plate", "status", "in_service_on"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if i >= len(w.Vehicles) {
				return nil, nil
			}
			v := w.Vehicles[i]
			i++
			inService := time.Date(v.Year, time.Month(1+v.Idx%12), 1+v.Idx%28, 0, 0, 0, 0, time.UTC)
			return []any{v.VIN, uid(v.Tenant.ID), uid(v.Fleet.ID), uid(v.Model.ID), int16(v.Year), v.Plate, "ACTIVE", inService}, nil
		})); err != nil {
		return err
	}
	i = 0
	from := o.Now.AddDate(0, 0, -90)
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"vehicle_assignment"},
		[]string{"tenant_id", "vin", "driver_id", "valid_from"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if i >= len(w.Vehicles) {
				return nil, nil
			}
			v := w.Vehicles[i]
			i++
			return []any{uid(v.Tenant.ID), v.VIN, uid(v.Driver.ID), from}, nil
		}))
	return err
}

// history: maintenance ground truth, trips and alerts derived from the same
// daily model that feeds ClickHouse.
func history(ctx context.Context, pool *pgxpool.Pool, w *sim.World, o Options, log *slog.Logger) error {
	today := day(o.Now)
	fromDay := float64(today - o.HistoryDays)
	// Service events: every breakdown in the history window plus its repair.
	var rows [][]any
	for _, v := range w.Vehicles {
		for _, e := range v.FailuresBetween(fromDay, float64(today)) {
			at := time.Unix(int64(e.Failure*86400), 0).UTC()
			rows = append(rows,
				[]any{uid(sim.DeterministicID("svc", fmt.Sprintf("%s/%s/%d/B", v.VIN, e.Component, int(e.Failure)))), uid(v.Tenant.ID), v.VIN,
					"BREAKDOWN", e.Component, at, sim.BreakdownCostUSD[e.Component], "Roadside breakdown; towed to depot"},
				[]any{uid(sim.DeterministicID("svc", fmt.Sprintf("%s/%s/%d/R", v.VIN, e.Component, int(e.Failure)))), uid(v.Tenant.ID), v.VIN,
					"REPAIR", e.Component, at.Add(time.Duration(e.RepairDays*24) * time.Hour), 0.0, "Component replaced"},
			)
		}
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"service_event"},
		[]string{"service_event_id", "tenant_id", "vin", "event_type", "component", "occurred_at", "cost_usd", "notes"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("service events: %w", err)
	}
	log.Info("seeded service events", "rows", len(rows))

	// Trips and alerts are streamed (millions of rows) with parallel COPYs.
	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, 2*workers)
	var tripsN, alertsN int64
	var mu sync.Mutex
	for wk := 0; wk < workers; wk++ {
		wg.Add(1)
		go func(wk int) {
			defer wg.Done()
			var buf [][]any
			var alerts [][]any
			for vi := wk; vi < len(w.Vehicles); vi += workers {
				v := w.Vehicles[vi]
				for d := today - o.AlertDays; d < today; d++ {
					row, ok := v.Daily(d)
					if !ok {
						continue
					}
					if d >= today-o.TripDays {
						for _, t := range v.TripsFor(row) {
							buf = append(buf, []any{uid(t.TripID), uid(v.Tenant.ID), v.VIN, uid(v.Driver.ID),
								time.UnixMilli(t.StartMs).UTC(), time.UnixMilli(t.EndMs).UTC(), t.DistanceKm, t.MaxSpeed,
								int32(t.IdleS), int32(t.HarshBrakes), int32(t.HarshAccels), int32(t.OverspeedS), t.EnergyUsed,
								stream.Geohash(t.StartLat, t.StartLon, 6), stream.Geohash(t.EndLat, t.EndLon, 6)})
						}
					}
					for ai, a := range v.AlertsFor(row) {
						ts := time.UnixMilli(a.TsMs).UTC()
						status, age := "OPEN", o.Now.Sub(ts)
						var ackAt, resAt *time.Time
						switch {
						case age > 72*time.Hour || (age > 24*time.Hour && a.Severity == "LOW"):
							t1, t2 := ts.Add(40*time.Minute), ts.Add(26*time.Hour)
							status, ackAt, resAt = "RESOLVED", &t1, &t2
						case age > 12*time.Hour && vi%3 == 0:
							t1 := ts.Add(2 * time.Hour)
							status, ackAt = "ACKNOWLEDGED", &t1
						}
						details, _ := json.Marshal(a.Details)
						alerts = append(alerts, []any{uid(sim.DeterministicID("alert", fmt.Sprintf("%s|%s|%d|%d", v.VIN, a.Type, a.TsMs, ai))),
							uid(v.Tenant.ID), v.VIN, a.Type, a.Severity, a.Title, status, ts, ts.Add(1500 * time.Millisecond),
							v.HomeLat, v.HomeLon, details, ackAt, resAt})
					}
				}
				if len(buf) >= 50_000 || vi+workers >= len(w.Vehicles) {
					if err := copyRows(ctx, pool, "trip", tripCols, buf); err != nil {
						errs <- err
						return
					}
					mu.Lock()
					tripsN += int64(len(buf))
					mu.Unlock()
					buf = buf[:0]
				}
				if len(alerts) >= 50_000 || vi+workers >= len(w.Vehicles) {
					if err := copyRows(ctx, pool, "alert", alertCols, alerts); err != nil {
						errs <- err
						return
					}
					mu.Lock()
					alertsN += int64(len(alerts))
					mu.Unlock()
					alerts = alerts[:0]
				}
			}
		}(wk)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	log.Info("seeded trips and alerts", "trips", tripsN, "alerts", alertsN)
	return nil
}

var tripCols = []string{"trip_id", "tenant_id", "vin", "driver_id", "start_ts", "end_ts", "distance_km", "max_speed_kmh",
	"idle_s", "harsh_brakes", "harsh_accels", "overspeed_s", "energy_used_pct", "start_geohash", "end_geohash"}
var alertCols = []string{"alert_id", "tenant_id", "vin", "alert_type", "severity", "title", "status", "ts", "detected_at",
	"lat", "lon", "details", "acknowledged_at", "resolved_at"}

func copyRows(ctx context.Context, pool *pgxpool.Pool, table string, cols []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := pool.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows))
	return err
}

// ClickHouse back-fills fleet.vehicle_daily for the history window.
func ClickHouse(ctx context.Context, ch *migrate.ClickHouse, w *sim.World, o Options, log *slog.Logger) error {
	today := day(o.Now)
	first := time.Unix(int64(today-o.HistoryDays)*86400, 0).UTC().Format("2006-01-02")
	cnt, err := ch.Query(ctx, fmt.Sprintf("SELECT count() FROM fleet.vehicle_daily WHERE day >= '%s' AND day < today()", first))
	if err != nil {
		return err
	}
	if n, _ := strconv.Atoi(cnt); n > len(w.Vehicles)*o.HistoryDays/3 && !o.Force {
		log.Info("clickhouse already seeded; skipping", "rows", n)
		return nil
	}
	if n, _ := strconv.Atoi(cnt); n > 0 {
		if err := ch.Exec(ctx, fmt.Sprintf("ALTER TABLE fleet.vehicle_daily DELETE WHERE day >= '%s' AND day < today() SETTINGS mutations_sync = 2", first), nil); err != nil {
			return err
		}
	}
	start := time.Now()
	cols := "tenant_id, vin, day, samples, ign_samples, idle_samples, moving_samples, odo_min, odo_max, max_speed, " +
		"max_coolant, sum_coolant, n_coolant, min_batt_v, sum_batt_v, n_batt_v, max_pack_temp, min_soc, harsh_events, " +
		"overspeed_samples, dtc_total, dtc_cooling, dtc_battery, dtc_misfire, dtc_evpack"
	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	var total int64
	var mu sync.Mutex
	for wk := 0; wk < workers; wk++ {
		wg.Add(1)
		go func(wk int) {
			defer wg.Done()
			pr, pw := io.Pipe()
			go func() {
				bw := bufio.NewWriterSize(pw, 1<<20)
				n := int64(0)
				for vi := wk; vi < len(w.Vehicles); vi += workers {
					v := w.Vehicles[vi]
					for d := today - o.HistoryDays; d < today; d++ {
						r, ok := v.Daily(d)
						if !ok {
							continue
						}
						writeTSV(bw, &r)
						n++
					}
				}
				mu.Lock()
				total += n
				mu.Unlock()
				_ = bw.Flush()
				_ = pw.Close()
			}()
			if err := ch.Exec(ctx, "INSERT INTO fleet.vehicle_daily ("+cols+") FORMAT TabSeparated", pr); err != nil {
				_ = pr.CloseWithError(err)
				errs <- err
			}
		}(wk)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	log.Info("clickhouse daily history seeded", "rows", total, "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}

func nf(p *float64) string {
	if p == nil {
		return `\N`
	}
	return strconv.FormatFloat(*p, 'f', 2, 64)
}

func writeTSV(bw *bufio.Writer, r *sim.DailyRow) {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
	u := func(v uint32) string { return strconv.FormatUint(uint64(v), 10) }
	fields := []string{r.TenantID, r.VIN, r.Day.Format("2006-01-02"), u(r.Samples), u(r.IgnSamples), u(r.IdleSamples),
		u(r.MovingSamples), f(r.OdoMin), f(r.OdoMax), f(r.MaxSpeed), nf(r.MaxCoolant), f(r.SumCoolant), u(r.NCoolant),
		nf(r.MinBattV), f(r.SumBattV), u(r.NBattV), nf(r.MaxPackTemp), nf(r.MinSoc), u(r.HarshEvents), u(r.OverspeedSmp),
		u(r.DTCTotal), u(r.DTCCooling), u(r.DTCBattery), u(r.DTCMisfire), u(r.DTCEVPack)}
	bw.WriteString(strings.Join(fields, "\t"))
	bw.WriteByte('\n')
}
