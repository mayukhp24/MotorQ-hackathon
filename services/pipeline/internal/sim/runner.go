package sim

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"fleetpulse/pipeline/internal/canonical"
)

// Publisher delivers one OEM batch to the platform (MQTT or HTTPS).
type Publisher interface {
	Publish(ctx context.Context, oem string, payload []byte) error
}

// RunnerConfig controls realism knobs of the delivery layer.
type RunnerConfig struct {
	Interval     time.Duration // sampling period per vehicle
	BatchSize    int           // records per OEM batch
	Workers      int
	ActiveShare  float64 // fraction of vehicles on the road
	DupRate      float64 // OEM retries → duplicate records
	OutOfOrder   float64 // records delayed by 1-10 s
	BadRate      float64 // malformed records (schema/VIN violations)
	BurstEvery   time.Duration
	BurstFor     time.Duration
	BurstFactor  int // sampling multiplier during a burst (shift start)
	OutageEvery  time.Duration
	OutageFor    time.Duration
	OutageShare  float64 // vehicles that lose connectivity and later flush
	Limit        int     // stop after N events (0 = forever)
	RateLimitEPS int     // optional global cap (events/s), 0 = none
}

func DefaultRunnerConfig() RunnerConfig {
	return RunnerConfig{Interval: time.Second, BatchSize: 500, Workers: 8, ActiveShare: 0.6,
		DupRate: 0.01, OutOfOrder: 0.02, BadRate: 0.0005, BurstFactor: 3, OutageShare: 0.1}
}

// Stats are exported as Prometheus metrics by the simulator binary.
type Stats struct {
	Events, Batches, Dups, Delayed, Bad, PublishErrors, Buffered atomic.Int64
}

type Runner struct {
	W     *World
	Cfg   RunnerConfig
	Pub   Publisher
	Stats Stats
	Log   *slog.Logger
	now   func() time.Time
}

func NewRunner(w *World, cfg RunnerConfig, pub Publisher, log *slog.Logger) *Runner {
	return &Runner{W: w, Cfg: cfg, Pub: pub, Log: log, now: time.Now}
}

type delayed struct {
	due time.Time
	e   canonical.Event
}

type batcher struct {
	enc  Encoder
	buf  []byte
	n    int
	oem  string
	size int
}

func (b *batcher) add(e *canonical.Event) bool {
	if b.n == 0 {
		b.buf = b.enc.Begin(b.buf[:0])
	}
	b.buf = b.enc.Record(b.buf, e, b.n == 0)
	b.n++
	return b.n >= b.size
}

func (b *batcher) take() []byte {
	out := b.enc.End(b.buf)
	cp := make([]byte, len(out))
	copy(cp, out)
	b.n = 0
	return cp
}

// inBurst reports whether t falls inside a periodic window.
func inWindow(start time.Time, t time.Time, every, dur time.Duration) bool {
	if every <= 0 || dur <= 0 {
		return false
	}
	return t.Sub(start)%every < dur
}

// Run streams telemetry until ctx is cancelled or Limit is reached.
func (r *Runner) Run(ctx context.Context) error {
	start := r.now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	shards := r.Cfg.Workers
	var limiter <-chan time.Time
	var tokens atomic.Int64
	if r.Cfg.RateLimitEPS > 0 {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		limiter = t.C
		go func() {
			for range limiter {
				tokens.Store(int64(r.Cfg.RateLimitEPS / 10))
			}
		}()
	}
	for s := 0; s < shards; s++ {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			r.worker(ctx, cancel, shard, shards, start, &tokens)
		}(s)
	}
	wg.Wait()
	return nil
}

func (r *Runner) worker(ctx context.Context, cancel context.CancelFunc, shard, shards int, start time.Time, tokens *atomic.Int64) {
	rng := rand.New(rand.NewPCG(uint64(shard), uint64(start.UnixNano())))
	nowSec := float64(r.now().UnixMilli()) / 1000
	var lives []*Live
	for i := shard; i < len(r.W.Vehicles); i += shards {
		lives = append(lives, NewLive(r.W.Vehicles[i], nowSec, r.Cfg.ActiveShare))
	}
	offline := make([]bool, len(lives))
	for i := range offline {
		offline[i] = rng.Float64() < r.Cfg.OutageShare
	}
	batchers := map[string]*batcher{}
	for _, o := range OEMs {
		batchers[o.Code] = &batcher{enc: EncoderFor(o.Code), oem: o.Code, size: r.Cfg.BatchSize}
	}
	var pending []delayed         // out-of-order / duplicate re-sends
	var backlog []canonical.Event // buffered during a simulated outage

	publish := func(b *batcher) {
		if b.n == 0 {
			return
		}
		n := int64(b.n)
		payload := b.take()
		for attempt := 0; ; attempt++ {
			err := r.Pub.Publish(ctx, b.oem, payload)
			if err == nil {
				r.Stats.Batches.Add(1)
				r.Stats.Events.Add(n)
				return
			}
			r.Stats.PublishErrors.Add(1)
			if ctx.Err() != nil || attempt >= 8 {
				r.Log.Warn("publish failed, dropping batch", "oem", b.oem, "records", n, "err", err)
				return
			}
			// Exponential back-off: back-pressure from the gateway slows the sender.
			time.Sleep(time.Duration(50<<attempt) * time.Millisecond)
		}
	}
	emit := func(e *canonical.Event) {
		b := batchers[e.OEM]
		if b.add(e) {
			publish(b)
		}
	}

	// Sub-ticks spread each shard's vehicles across the interval to avoid a
	// synchronized thundering herd every second.
	const slices = 10
	sub := r.Cfg.Interval / slices
	ticker := time.NewTicker(sub)
	defer ticker.Stop()
	slice := 0
	for {
		select {
		case <-ctx.Done():
			for _, b := range batchers {
				publish(b)
			}
			return
		case <-ticker.C:
		}
		now := r.now()
		nowSec := float64(now.UnixMilli()) / 1000
		factor := 1
		if inWindow(start, now, r.Cfg.BurstEvery, r.Cfg.BurstFor) && r.Cfg.BurstFactor > 1 {
			factor = r.Cfg.BurstFactor
		}
		outage := inWindow(start, now, r.Cfg.OutageEvery, r.Cfg.OutageFor)
		if !outage && len(backlog) > 0 { // connectivity restored: flush buffered samples
			for i := range backlog {
				emit(&backlog[i])
			}
			backlog = backlog[:0]
			r.Stats.Buffered.Store(0)
		}
		dt := r.Cfg.Interval.Seconds() / float64(factor)
		for i := slice; i < len(lives); i += slices {
			for k := 0; k < factor; k++ {
				ts := nowSec - dt*float64(factor-1-k)
				e := lives[i].Step(ts, dt)
				if outage && offline[i] {
					backlog = append(backlog, e)
					r.Stats.Buffered.Add(1)
					continue
				}
				switch x := rng.Float64(); {
				case x < r.Cfg.BadRate:
					corrupt(&e, rng)
					r.Stats.Bad.Add(1)
				case x < r.Cfg.BadRate+r.Cfg.OutOfOrder:
					pending = append(pending, delayed{now.Add(time.Duration(1+rng.IntN(10)) * time.Second), e})
					r.Stats.Delayed.Add(1)
					continue
				case x < r.Cfg.BadRate+r.Cfg.OutOfOrder+r.Cfg.DupRate:
					pending = append(pending, delayed{now.Add(time.Duration(rng.IntN(5)) * time.Second), e})
					r.Stats.Dups.Add(1)
				}
				emit(&e)
			}
		}
		// Release delayed/duplicate records whose time has come.
		kept := pending[:0]
		for _, d := range pending {
			if !now.Before(d.due) {
				e := d.e
				emit(&e)
			} else {
				kept = append(kept, d)
			}
		}
		pending = kept
		for _, b := range batchers {
			publish(b)
		}
		slice = (slice + 1) % slices
		if r.Cfg.Limit > 0 && r.Stats.Events.Load() >= int64(r.Cfg.Limit) {
			cancel()
		}
		if r.Cfg.RateLimitEPS > 0 {
			for tokens.Add(-int64(len(lives)/slices*factor)) < 0 && ctx.Err() == nil {
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
}

// corrupt injects realistic data-quality faults.
func corrupt(e *canonical.Event, r *rand.Rand) {
	switch r.IntN(3) {
	case 0: // wrong check digit
		b := []byte(e.VIN)
		if b[8] == '0' {
			b[8] = '1'
		} else {
			b[8] = '0'
		}
		e.VIN = string(b)
	case 1: // impossible reading
		e.SpeedKmh = 999
	default: // timestamp from a badly configured clock
		e.TsMs += 3600 * 1000
	}
}
