// Package pipeline connects the CampusPulse processing stages with typed
// channels:
//
//	CampusSource ──ObservationBatch──► worker pool (validate → normalise → privacy hash → pre-aggregate)
//	             ──TelemetryBatch───►───────────────────────────────────────────────┐
//	             ──SimFrame─────────►─────────────────────────────────────────────┐ │
//	worker pool ──Result[Partial]──► aggregator ──AggregatedTick──► (fusion, Stage 3)
//
// Channel ownership: each channel is closed by the goroutine that sends on
// it, and only after it has finished sending. Closure cascades downstream,
// so cancelling the context drains and stops every stage without leaks.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/fn"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
	"github.com/dylanpanashe/csci_6221/internal/workerpool"
)

// AggregatedTick is the de-duplicated device picture for one tick. It still
// holds exact small counts, so it is internal: the publication step applies
// suppression and noise before anything reaches a client.
type AggregatedTick struct {
	Seq      uint64
	At       time.Time
	Rooms    map[campus.RoomID]campus.DeviceCount
	APs      map[campus.AccessPointID]APObserved
	Accepted int
	Rejected int
	Batches  int
	Elapsed  time.Duration // real time from first batch to completion
}

// Config sizes the pipeline.
type Config struct {
	Workers int
	Buffer  int
}

// Stats is a snapshot of pipeline counters.
type Stats struct {
	Pool         workerpool.Snapshot `json:"pool"`
	Accepted     int64               `json:"accepted"`
	Rejected     int64               `json:"rejected"`
	Ticks        int64               `json:"ticks"`
	DroppedTicks int64               `json:"dropped_ticks"`
	LastTickMs   float64             `json:"last_tick_ms"`
}

// Outputs are the receive-only ends handed to downstream stages.
type Outputs struct {
	Aggregates <-chan AggregatedTick
	Telemetry  <-chan campus.TelemetryBatch
	Frames     <-chan campus.SimFrame
	// SourceErr receives the source's exit error (or nil) once, then closes.
	SourceErr <-chan error
}

// Pipeline is the ingest half of CampusPulse.
type Pipeline struct {
	cfg  Config
	log  *slog.Logger
	pool *workerpool.Pool[campus.ObservationBatch, Partial]

	accepted, rejected, ticks, dropped atomic.Int64
	lastTickNs                         atomic.Int64
}

// New builds the pipeline and its worker pool.
func New(cfg Config, c *campus.Campus, h *privacy.Hasher, log *slog.Logger) (*Pipeline, error) {
	if cfg.Buffer < 1 {
		cfg.Buffer = 64
	}
	// Function composition: three independent steps become one.
	chain := fn.Chain(ValidateStep(c), NormalizeStep(c), PrivacyStep(h))
	pool, err := workerpool.New("observations", cfg.Workers, ProcessBatch(chain))
	if err != nil {
		return nil, err
	}
	return &Pipeline{cfg: cfg, log: log, pool: pool}, nil
}

// Start launches the source, the worker pool and the aggregator. Every
// goroutine it starts exits once ctx is cancelled and the channels drain.
func (p *Pipeline) Start(ctx context.Context, src campus.CampusSource) Outputs {
	obs := make(chan campus.ObservationBatch, p.cfg.Buffer)
	tel := make(chan campus.TelemetryBatch, 4)
	frames := make(chan campus.SimFrame, 4)
	srcErr := make(chan error, 1)

	// Producer: the source writes to send-only ends. This goroutine is the
	// sole owner of the three channels and closes them when Run returns.
	go func() {
		defer close(srcErr)
		defer close(frames)
		defer close(tel)
		defer close(obs)
		srcErr <- src.Run(ctx, campus.Feeds{Observations: obs, Telemetry: tel, Frames: frames})
	}()

	results := p.pool.Run(ctx, obs) // fan-out to N workers, fan-in to one channel
	aggs := make(chan AggregatedTick, 4)
	go func() {
		defer close(aggs)
		p.aggregate(ctx, results, aggs)
	}()

	return Outputs{Aggregates: aggs, Telemetry: tel, Frames: frames, SourceErr: srcErr}
}

// pendingTick accumulates partial results for one tick.
type pendingTick struct {
	at      time.Time
	started time.Time
	got     int
	total   int
	rooms   map[campus.RoomID]map[campus.DeviceHash]struct{}
	aps     map[campus.AccessPointID]*apMerge
	acc     int
	rej     int
}

type apMerge struct {
	set       map[campus.DeviceHash]struct{}
	signalSum float64
	n         int
	weak      int
}

// aggregate merges partials by tick sequence number. It emits a tick once
// all of its batches have arrived, in whatever order the workers finished.
func (p *Pipeline) aggregate(ctx context.Context, in <-chan workerpool.Result[Partial], out chan<- AggregatedTick) {
	pending := map[uint64]*pendingTick{}
	for res := range in {
		if res.Err != nil {
			var pe *workerpool.PanicError
			if errors.As(res.Err, &pe) {
				p.log.Error("observation worker panicked; batch dropped", "panic", pe.Value)
			} else if !errors.Is(res.Err, context.Canceled) {
				p.log.Warn("observation batch failed", "err", res.Err)
			}
			continue
		}
		part := res.Value
		pt := pending[part.Seq]
		if pt == nil {
			pt = &pendingTick{at: part.At, started: time.Now(), total: part.Total,
				rooms: map[campus.RoomID]map[campus.DeviceHash]struct{}{}, aps: map[campus.AccessPointID]*apMerge{}}
			pending[part.Seq] = pt
		}
		pt.merge(part)
		if pt.got < pt.total {
			continue
		}
		delete(pending, part.Seq)
		// Older ticks still waiting can never complete (a batch was lost).
		for seq := range pending {
			if seq < part.Seq {
				delete(pending, seq)
				p.dropped.Add(1)
			}
		}
		tick := pt.finish(part.Seq)
		p.accepted.Add(int64(tick.Accepted))
		p.rejected.Add(int64(tick.Rejected))
		p.ticks.Add(1)
		p.lastTickNs.Store(int64(tick.Elapsed))
		select {
		case out <- tick:
		case <-ctx.Done():
			return
		}
	}
}

func (pt *pendingTick) merge(part Partial) {
	pt.got++
	pt.acc += part.Accepted
	pt.rej += part.Rejected
	for room, hs := range part.Rooms {
		set := pt.rooms[room]
		if set == nil {
			set = make(map[campus.DeviceHash]struct{}, len(hs))
			pt.rooms[room] = set
		}
		for _, h := range hs {
			set[h] = struct{}{}
		}
	}
	for ap, a := range part.APs {
		m := pt.aps[ap]
		if m == nil {
			m = &apMerge{set: make(map[campus.DeviceHash]struct{}, len(a.hashes))}
			pt.aps[ap] = m
		}
		for _, h := range a.hashes {
			m.set[h] = struct{}{}
		}
		m.signalSum += a.signalSum
		m.n += len(a.hashes)
		m.weak += a.weak
	}
}

func (pt *pendingTick) finish(seq uint64) AggregatedTick {
	t := AggregatedTick{Seq: seq, At: pt.at, Accepted: pt.acc, Rejected: pt.rej, Batches: pt.total,
		Rooms:   make(map[campus.RoomID]campus.DeviceCount, len(pt.rooms)),
		APs:     make(map[campus.AccessPointID]APObserved, len(pt.aps)),
		Elapsed: time.Since(pt.started)}
	for room, set := range pt.rooms {
		t.Rooms[room] = campus.DeviceCount(len(set))
	}
	for ap, m := range pt.aps {
		o := APObserved{Devices: campus.DeviceCount(len(m.set))}
		if m.n > 0 {
			o.AvgSignal = campus.SignalStrength(m.signalSum / float64(m.n))
			o.WeakShare = float64(m.weak) / float64(m.n)
		}
		t.APs[ap] = o
	}
	return t
}

// Stats returns current counters.
func (p *Pipeline) Stats() Stats {
	return Stats{
		Pool:         p.pool.Stats(),
		Accepted:     p.accepted.Load(),
		Rejected:     p.rejected.Load(),
		Ticks:        p.ticks.Load(),
		DroppedTicks: p.dropped.Load(),
		LastTickMs:   float64(p.lastTickNs.Load()) / 1e6,
	}
}
