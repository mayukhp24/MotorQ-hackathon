// Package processor is the real-time stream processor: it de-duplicates,
// enriches and evaluates every canonical event, maintains live per-vehicle
// state and emits alerts and trips. State is partitioned exactly like the
// Kafka topic (key = VIN), so instances scale horizontally with partitions.
package processor

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	json "github.com/goccy/go-json"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"fleetpulse/pipeline/internal/canonical"
	"fleetpulse/pipeline/internal/detect"
	"fleetpulse/pipeline/internal/stream"
)

// ---- Ports (hexagonal architecture): implemented by Kafka/Redis/Postgres
// adapters in production and by fakes in tests.

// MetaSource resolves VIN → tenant/fleet/driver reference data.
type MetaSource interface {
	Get(vin string) (detect.VehicleMeta, bool)
}

// Output publishes derived records (Kafka).
type Output interface {
	Enriched(key string, v []byte)
	Alert(key string, v []byte)
	Trip(key string, v []byte)
	DeadLetter(key string, v []byte)
}

// LiveRecord is the hot live-state row for one vehicle.
type LiveRecord struct {
	VIN, TenantID, FleetID, Status, Powertrain, Geohash string
	Lat, Lon, Speed, Heading, Odo                       float64
	Soc, Fuel, Coolant, BattV, PackTemp                 *float64
	TsMs                                                int64
	Ignition                                            bool
	LastAlert, LastAlertSev                             string
	CriticalUntilMs                                     int64
}

// LiveStore is the low-latency read model (Redis).
type LiveStore interface {
	WriteVehicles(ctx context.Context, recs []LiveRecord) error
	PublishAlert(ctx context.Context, tenant string, payload []byte) error
	WriteSnapshot(ctx context.Context, key string, field string, payload []byte) error
}

var (
	mEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "processor_events_total", Help: "Events by outcome"}, []string{"result"})
	mAlerts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "processor_alerts_total", Help: "Alerts raised"}, []string{"type", "severity"})
	mTrips  = promauto.NewCounter(prometheus.CounterOpts{Name: "processor_trips_total", Help: "Trips closed"})
	mE2E    = promauto.NewHistogram(prometheus.HistogramOpts{Name: "processor_event_age_seconds", Help: "Vehicle timestamp to processing time", Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60}})
	mIngest = promauto.NewHistogram(prometheus.HistogramOpts{Name: "processor_ingest_to_process_seconds", Help: "Gateway receipt to processing time", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2, 5}})
	mAlertL = promauto.NewHistogram(prometheus.HistogramOpts{Name: "processor_alert_latency_seconds", Help: "Vehicle timestamp to alert publication", Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10}})
	mLiveW  = promauto.NewCounter(prometheus.CounterOpts{Name: "processor_live_writes_total", Help: "Live-state rows written"})
	mParts  = promauto.NewGauge(prometheus.GaugeOpts{Name: "processor_partitions_owned", Help: "Partitions owned by this instance"})
	mVeh    = promauto.NewGauge(prometheus.GaugeOpts{Name: "processor_vehicles_tracked", Help: "Vehicles with in-memory state"})
)

type vehState struct {
	det       *detect.State
	meta      detect.VehicleMeta
	last      canonical.Event
	gh        string
	dirty     bool
	written   int64
	status    string
	lastAl    string
	lastSev   string
	critUntil int64
}

type partState struct {
	mu       sync.Mutex
	vehicles map[string]*vehState
	dedup    *stream.Dedup
	topk     map[string]*stream.TopK
	topkFrom time.Time
	events   int64
	snapAt   time.Time
}

// Config for the processor.
type Config struct {
	Instance         string
	DedupPerPart     int
	DedupWindow      time.Duration
	WriteMovingMs    int64
	WriteIdleMs      int64
	CriticalHoldMs   int64
	TopKWindow       time.Duration
	OnlineWindowMs   int64
	ClusterPrecision []int
}

func DefaultConfig() Config {
	return Config{Instance: "local", DedupPerPart: 2_000_000, DedupWindow: 2 * time.Minute,
		WriteMovingMs: 1000, WriteIdleMs: 15_000, CriticalHoldMs: 30 * 60_000,
		TopKWindow: 15 * time.Minute, OnlineWindowMs: 5 * 60_000, ClusterPrecision: []int{3, 4, 5}}
}

type Processor struct {
	cfg    Config
	eng    *detect.Engine
	meta   MetaSource
	out    Output
	live   LiveStore
	log    *slog.Logger
	now    func() time.Time
	mu     sync.RWMutex
	parts  map[int32]*partState
	events atomic.Int64
}

func New(cfg Config, eng *detect.Engine, meta MetaSource, out Output, live LiveStore, log *slog.Logger) *Processor {
	return &Processor{cfg: cfg, eng: eng, meta: meta, out: out, live: live, log: log, now: time.Now, parts: map[int32]*partState{}}
}

func (p *Processor) part(id int32) *partState {
	p.mu.RLock()
	ps := p.parts[id]
	p.mu.RUnlock()
	if ps != nil {
		return ps
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ps = p.parts[id]; ps == nil {
		ps = &partState{vehicles: map[string]*vehState{}, dedup: stream.NewDedup(p.cfg.DedupPerPart, 0.001, p.cfg.DedupWindow),
			topk: map[string]*stream.TopK{}, topkFrom: p.now(), snapAt: p.now()}
		p.parts[id] = ps
		mParts.Set(float64(len(p.parts)))
	}
	return ps
}

// Revoke drops state for partitions moved to another instance (rebalance).
func (p *Processor) Revoke(ids []int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		delete(p.parts, id)
	}
	mParts.Set(float64(len(p.parts)))
}

// Owned returns the partitions this instance currently holds.
func (p *Processor) Owned() []int32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]int32, 0, len(p.parts))
	for id := range p.parts {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Process handles one partition's slice of records in order.
func (p *Processor) Process(ctx context.Context, partition int32, values [][]byte) {
	ps := p.part(partition)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, raw := range values {
		p.processOne(ctx, ps, raw)
	}
}

func (p *Processor) processOne(ctx context.Context, ps *partState, raw []byte) {
	var e canonical.Event
	if err := json.Unmarshal(raw, &e); err != nil {
		mEvents.WithLabelValues("undecodable").Inc()
		p.out.DeadLetter("processor", raw)
		return
	}
	if ps.dedup.Seen(e.EventID) {
		mEvents.WithLabelValues("duplicate").Inc()
		return
	}
	meta, ok := p.meta.Get(e.VIN)
	if !ok {
		// Not provisioned to any tenant: never store data we cannot attribute.
		mEvents.WithLabelValues("unknown_vehicle").Inc()
		b, _ := json.Marshal(map[string]any{"oem": e.OEM, "reason": "vehicle not provisioned", "raw": string(raw)})
		p.out.DeadLetter(e.VIN, b)
		return
	}
	now := p.now()
	nowMs := now.UnixMilli()
	vs := ps.vehicles[e.VIN]
	if vs == nil {
		vs = &vehState{det: detect.NewState(), meta: meta}
		ps.vehicles[e.VIN] = vs
	}
	vs.meta = meta
	res := p.eng.Process(vs.det, &e, meta, nowMs)
	gh := stream.Geohash(e.Lat, e.Lon, 7)

	enr := canonical.Enriched{Event: e, TenantID: meta.TenantID, FleetID: meta.FleetID, Geohash: gh,
		Powertrain: meta.Powertrain, ProcMs: nowMs}
	if b, err := json.Marshal(&enr); err == nil {
		p.out.Enriched(e.VIN, b)
	}
	for i := range res.Alerts {
		a := &res.Alerts[i]
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		p.out.Alert(e.VIN, b)
		if err := p.live.PublishAlert(ctx, meta.TenantID, b); err != nil {
			p.log.Debug("alert publish failed", "err", err)
		}
		mAlerts.WithLabelValues(a.Type, a.Severity).Inc()
		mAlertL.Observe(float64(nowMs-a.TsMs) / 1000)
		vs.lastAl, vs.lastSev = a.Type, a.Severity
		if a.Severity == detect.SevCritical {
			vs.critUntil = a.TsMs + p.cfg.CriticalHoldMs
		}
	}
	if res.Trip != nil {
		if b, err := json.Marshal(res.Trip); err == nil {
			p.out.Trip(e.VIN, b)
			mTrips.Inc()
		}
	}
	for _, c := range e.DTC {
		tk := ps.topk[meta.TenantID]
		if tk == nil {
			tk = stream.NewTopK(10, 0.001, 0.01)
			ps.topk[meta.TenantID] = tk
		}
		tk.Add(c)
	}
	if res.Late {
		mEvents.WithLabelValues("late").Inc()
	} else {
		mEvents.WithLabelValues("processed").Inc()
		vs.last, vs.gh, vs.dirty = e, gh, true
	}
	ps.events++
	p.events.Add(1)
	mE2E.Observe(float64(nowMs-e.TsMs) / 1000)
	if e.IngestMs > 0 {
		mIngest.Observe(float64(nowMs-e.IngestMs) / 1000)
	}
}

// FlushLive writes changed vehicles to the live store. Moving vehicles are
// written at most every WriteMovingMs; parked ones every WriteIdleMs, which
// cuts Redis writes for the ~40% of the fleet that is stationary.
func (p *Processor) FlushLive(ctx context.Context) error {
	nowMs := p.now().UnixMilli()
	var recs []LiveRecord
	for _, id := range p.Owned() {
		ps := p.part(id)
		ps.mu.Lock()
		for vin, vs := range ps.vehicles {
			if !vs.dirty {
				continue
			}
			st := vs.det.Status
			moving := st == detect.StatusDriving
			age := nowMs - vs.written
			if st == vs.status && ((moving && age < p.cfg.WriteMovingMs) || (!moving && age < p.cfg.WriteIdleMs)) {
				continue
			}
			e := &vs.last
			recs = append(recs, LiveRecord{
				VIN: vin, TenantID: vs.meta.TenantID, FleetID: vs.meta.FleetID, Status: st,
				Powertrain: vs.meta.Powertrain, Geohash: vs.gh, Lat: e.Lat, Lon: e.Lon, Speed: e.SpeedKmh,
				Heading: e.Heading, Odo: e.OdoKm, Soc: e.SocPct, Fuel: e.FuelPct, Coolant: e.CoolantC,
				BattV: e.BattV, PackTemp: e.PackTempC, TsMs: e.TsMs, Ignition: e.Ignition,
				LastAlert: vs.lastAl, LastAlertSev: vs.lastSev, CriticalUntilMs: vs.critUntil,
			})
			vs.dirty, vs.written, vs.status = false, nowMs, st
		}
		ps.mu.Unlock()
	}
	if len(recs) == 0 {
		return nil
	}
	mLiveW.Add(float64(len(recs)))
	return p.live.WriteVehicles(ctx, recs)
}

// KPI is a per-tenant, per-partition live summary; the API sums partitions.
type KPI struct {
	TsMs      int64          `json:"ts_ms"`
	Online    int            `json:"online"`
	ByStatus  map[string]int `json:"by_status"`
	Critical  int            `json:"critical"`
	EPS       float64        `json:"eps"`
	SpeedSum  float64        `json:"speed_sum"`
	Moving    int            `json:"moving"`
	LowEnergy int            `json:"low_energy"`
}

// Snapshot publishes KPI, cluster and top-K DTC summaries for every owned
// partition. O(vehicles) every few seconds.
func (p *Processor) Snapshot(ctx context.Context) error {
	now := p.now()
	nowMs := now.UnixMilli()
	for _, id := range p.Owned() {
		ps := p.part(id)
		ps.mu.Lock()
		elapsed := now.Sub(ps.snapAt).Seconds()
		kpis := map[string]*KPI{}
		clusters := map[string]map[int]map[string]int{}
		for _, vs := range ps.vehicles {
			t := vs.meta.TenantID
			k := kpis[t]
			if k == nil {
				k = &KPI{TsMs: nowMs, ByStatus: map[string]int{}}
				kpis[t] = k
				clusters[t] = map[int]map[string]int{}
			}
			if nowMs-vs.last.TsMs > p.cfg.OnlineWindowMs {
				continue
			}
			k.Online++
			k.ByStatus[vs.det.Status]++
			if vs.critUntil > nowMs {
				k.Critical++
			}
			if vs.det.Status == detect.StatusDriving {
				k.Moving++
				k.SpeedSum += vs.last.SpeedKmh
			}
			if (vs.last.SocPct != nil && *vs.last.SocPct < 20) || (vs.last.FuelPct != nil && *vs.last.FuelPct < 10) {
				k.LowEnergy++
			}
			for _, prec := range p.cfg.ClusterPrecision {
				if len(vs.gh) >= prec {
					m := clusters[t][prec]
					if m == nil {
						m = map[string]int{}
						clusters[t][prec] = m
					}
					m[vs.gh[:prec]]++
				}
			}
		}
		// Event rate is attributed to tenants proportionally to online vehicles.
		totalOnline := 0
		for _, k := range kpis {
			totalOnline += k.Online
		}
		if elapsed > 0 && totalOnline > 0 {
			for _, k := range kpis {
				k.EPS = float64(ps.events) / elapsed * float64(k.Online) / float64(totalOnline)
			}
		}
		ps.events, ps.snapAt = 0, now
		topk := map[string][]stream.Item{}
		for t, tk := range ps.topk {
			topk[t] = tk.Items()
		}
		if now.Sub(ps.topkFrom) >= p.cfg.TopKWindow {
			ps.topk, ps.topkFrom = map[string]*stream.TopK{}, now
		}
		ps.mu.Unlock()

		field := strconv.Itoa(int(id))
		for t, k := range kpis {
			if b, err := json.Marshal(k); err == nil {
				if err := p.live.WriteSnapshot(ctx, "kpi:"+t, field, b); err != nil {
					return err
				}
			}
			for prec, m := range clusters[t] {
				b, _ := json.Marshal(map[string]any{"ts_ms": nowMs, "cells": m})
				if err := p.live.WriteSnapshot(ctx, "clu:"+t+":"+strconv.Itoa(prec), field, b); err != nil {
					return err
				}
			}
		}
		for t, items := range topk {
			b, _ := json.Marshal(map[string]any{"ts_ms": nowMs, "items": items})
			if err := p.live.WriteSnapshot(ctx, "topdtc:"+t, field, b); err != nil {
				return err
			}
		}
	}
	n := 0
	for _, id := range p.Owned() {
		ps := p.part(id)
		ps.mu.Lock()
		n += len(ps.vehicles)
		ps.mu.Unlock()
	}
	mVeh.Set(float64(n))
	return nil
}

// Events returns the total processed count (for logs/tests).
func (p *Processor) Events() int64 { return p.events.Load() }
