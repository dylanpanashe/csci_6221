// Package engine runs everything downstream of the ingest pipeline:
//
//	Aggregates ─► fusion ─┬─► store (estimates) ─► Updates bus
//	                      ├─► AP observations ─┐
//	                      ├─► forecast ticks ──┼─► forecaster (worker pool) ─► store, bus
//	                      └─► history ticks ───┼─► history collector ─► store
//	Telemetry ───────────────────────────────►─┴─► diagnostics ─► alerts ─► store, bus
//	Frames ──────────────────────────────────► frame relay ─► store, bus
//
// Each arrow is a typed channel with one sending goroutine, which closes it
// when its own input closes. Cancelling the context or closing the
// pipeline outputs shuts the whole graph down in order.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/accuracy"
	"github.com/dylanpanashe/csci_6221/internal/alerts"
	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
	"github.com/dylanpanashe/csci_6221/internal/events"
	"github.com/dylanpanashe/csci_6221/internal/forecast"
	"github.com/dylanpanashe/csci_6221/internal/occupancy"
	"github.com/dylanpanashe/csci_6221/internal/pipeline"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
	"github.com/dylanpanashe/csci_6221/internal/store"
	"github.com/dylanpanashe/csci_6221/internal/workerpool"
)

// SimView is the slice of the simulator the engine may use: the published
// timetable, and ground truth for the accuracy evaluator only.
type SimView interface {
	Schedule() *simulator.Schedule
	Truth() *simulator.Truth
}

// Config tunes the engine.
type Config struct {
	DevicesPerUser  float64
	SuppressBelow   int
	Noise           float64
	Seed            uint64
	Weights         occupancy.Weights
	Thresholds      diagnostics.Thresholds
	Strategy        string
	ForecastWorkers int
	ChangeoverMin   int // smallest class that triggers a changeover warning
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{DevicesPerUser: 1.65, SuppressBelow: 10, Noise: 1, Weights: occupancy.DefaultWeights,
		Thresholds: diagnostics.DefaultThresholds, Strategy: "rule_based", ForecastWorkers: 4, ChangeoverMin: 80}
}

// Engine owns the processing stages after ingest.
type Engine struct {
	cfg    Config
	campus *campus.Campus
	sim    SimView
	store  *store.Store
	fc     *forecast.Engine
	eval   *accuracy.Evaluator
	log    *slog.Logger

	// Updates carries every message destined for browsers.
	Updates *events.Bus[campus.Envelope]

	obsRate atomic.Uint64 // observations per real second, as float64 bits
}

// New wires up the engine.
func New(cfg Config, c *campus.Campus, sim SimView, st *store.Store, log *slog.Logger) (*Engine, error) {
	e := &Engine{cfg: cfg, campus: c, sim: sim, store: st, eval: accuracy.New(c), log: log,
		Updates: events.NewBus[campus.Envelope]()}
	fc, err := forecast.NewEngine(c, forecast.ProviderFunc(e.snapshot), cfg.Strategy)
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	e.fc = fc
	return e, nil
}

// Forecaster exposes the forecast engine to the API.
func (e *Engine) Forecaster() campus.ForecastEngine { return e.fc }

// AttendanceRatio is the learned observed/timetabled attendance ratio.
func (e *Engine) AttendanceRatio() float64 { return e.fc.AttendanceRatio() }

// Accuracy returns the current scorecard.
func (e *Engine) Accuracy() accuracy.Report {
	return e.eval.Report(math.Float64frombits(e.obsRate.Load()))
}

// snapshot builds a forecast starting point from the store.
func (e *Engine) snapshot() (forecast.Snapshot, error) {
	people, at := e.store.People()
	if at.IsZero() {
		return forecast.Snapshot{}, fmt.Errorf("no estimates yet: %w", campus.ErrNotFound)
	}
	return forecast.Snapshot{At: at, People: people, Timetable: e.sim.Schedule()}, nil
}

type tickNote struct {
	seq uint64
	at  time.Time
}

// Run processes pipeline outputs until they close. It returns when every
// stage has finished.
func (e *Engine) Run(ctx context.Context, in pipeline.Outputs) {
	apObs := make(chan map[campus.AccessPointID]diagnostics.Observed, 1)
	fcTicks := make(chan tickNote, 1)
	histTicks := make(chan tickNote, 1)

	var wg sync.WaitGroup
	run := func(name string, f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				// A bug in one stage is logged; the others keep running
				// until shutdown.
				if r := recover(); r != nil {
					e.log.Error("engine stage panicked", "stage", name, "panic", r)
				}
			}()
			f()
		}()
	}
	run("fusion", func() { e.fusionLoop(in.Aggregates, apObs, fcTicks, histTicks) })
	run("diagnostics", func() { e.diagnosticsLoop(in.Telemetry, apObs) })
	run("frames", func() { e.frameLoop(in.Frames) })
	run("forecast", func() { e.forecastLoop(ctx, fcTicks) })
	run("history", func() { e.historyLoop(histTicks) })
	run("source", func() {
		for err := range in.SourceErr {
			if err != nil {
				e.log.Error("campus source stopped", "err", err)
			}
		}
	})
	wg.Wait()
	e.Updates.Close()
}

// offer does a non-blocking, latest-wins send on a 1-buffered channel:
// if the consumer has not taken the previous value, it is replaced.
func offer[T any](ch chan T, v T) {
	for {
		select {
		case ch <- v:
			return
		default:
			select {
			case <-ch: // discard the stale value and retry
			default:
			}
		}
	}
}

func discontinuity(prev, now time.Time) bool {
	if prev.IsZero() {
		return false
	}
	d := now.Sub(prev)
	return d < -time.Minute || d > 30*time.Minute
}

// ---------------------------------------------------------------------------
// Fusion
// ---------------------------------------------------------------------------

func (e *Engine) fusionLoop(in <-chan pipeline.AggregatedTick, apObs chan map[campus.AccessPointID]diagnostics.Observed,
	fcTicks, histTicks chan tickNote) {
	defer close(apObs)
	defer close(fcTicks)
	defer close(histTicks)

	fusion := occupancy.NewFusion(e.cfg.Weights, e.store.APOffline)
	pub := privacy.NewPublisher(e.cfg.SuppressBelow, e.cfg.Noise, e.cfg.Seed+11)
	var last time.Time
	for t := range in {
		if discontinuity(last, t.At) {
			fusion.Reset()
			e.store.ResetHistory()
		}
		last = t.At
		sched := e.sim.Schedule()

		rooms := e.campus.AllRooms()
		raw := make(map[campus.RoomID]float64, len(rooms))
		rawEst := make([]campus.OccupancyEstimate, 0, len(rooms))
		published := make([]campus.OccupancyEstimate, 0, len(rooms))
		for _, r := range rooms {
			est := fusion.Estimate(campus.EstimateInput{Room: r, Devices: t.Rooms[r.ID],
				ActiveEvents: sched.EventsAt(r.ID, t.At), DevicesPerUser: e.cfg.DevicesPerUser, At: t.At})
			raw[r.ID] = float64(est.Count.Value)
			rawEst = append(rawEst, est)
			published = append(published, occupancy.Publish(est, pub))
		}
		groups := e.groups(rawEst, pub)
		e.store.PutEstimates(published)
		e.store.PutGroups(groups)
		e.store.PutPeople(t.At, raw)
		e.fc.Learn(t.At, raw, sched)

		if truth := e.sim.Truth(); truth != nil && truth.Seq == t.Seq {
			e.eval.ScoreOccupancy(raw, truth)
		}
		snap := forecast.Snapshot{At: t.At, People: raw, Timetable: sched}
		for _, model := range []string{"rule_based", "persistence"} {
			e.eval.RecordForecast(model, t.At.Add(time.Hour), e.fc.Predict(snap, time.Hour, model))
		}

		obs := make(map[campus.AccessPointID]diagnostics.Observed, len(t.APs))
		for id, o := range t.APs {
			obs[id] = diagnostics.Observed{Devices: int(o.Devices), AvgSignal: float64(o.AvgSignal), WeakShare: o.WeakShare}
		}
		offer(apObs, obs)
		offer(fcTicks, tickNote{t.Seq, t.At})
		offer(histTicks, tickNote{t.Seq, t.At})

		e.Updates.Publish(campus.Envelope{Type: "occupancy", At: t.At, Data: OccupancyMessage{
			Rooms: published, Groups: groups}})
	}
}

// OccupancyMessage is the WebSocket payload for occupancy updates.
type OccupancyMessage struct {
	Rooms  []campus.OccupancyEstimate          `json:"rooms"`
	Groups map[string]campus.OccupancyEstimate `json:"groups"`
}

// groups totals raw room estimates per floor, building and campus, then
// publishes each total through the privacy layer.
func (e *Engine) groups(raw []campus.OccupancyEstimate, pub *privacy.Publisher) map[string]campus.OccupancyEstimate {
	byID := make(map[campus.RoomID]campus.OccupancyEstimate, len(raw))
	for _, r := range raw {
		byID[r.RoomID] = r
	}
	out := map[string]campus.OccupancyEstimate{}
	var all []campus.OccupancyEstimate
	for _, b := range e.campus.Buildings {
		var inB []campus.OccupancyEstimate
		for _, f := range b.Floors {
			var inF []campus.OccupancyEstimate
			for _, r := range append(append([]*campus.Room(nil), f.Rooms...), f.Lobby) {
				inF = append(inF, byID[r.ID])
			}
			out[string(f.ID)] = occupancy.Publish(occupancy.Aggregate(string(f.ID), inF), pub)
			inB = append(inB, inF...)
		}
		out[string(b.ID)] = occupancy.Publish(occupancy.Aggregate(string(b.ID), inB), pub)
		all = append(all, inB...)
	}
	out["campus"] = occupancy.Publish(occupancy.Aggregate("campus", all), pub)
	return out
}

// ---------------------------------------------------------------------------
// Diagnostics and alerts
// ---------------------------------------------------------------------------

func (e *Engine) diagnosticsLoop(tel <-chan campus.TelemetryBatch, apObs <-chan map[campus.AccessPointID]diagnostics.Observed) {
	diag := diagnostics.NewEngine(e.campus, diagnostics.DefaultRules(e.cfg.Thresholds), 3)
	mgr := alerts.NewManager(e.campus, e.store, 2)
	var last time.Time
	for tel != nil {
		select {
		case o, ok := <-apObs:
			if !ok {
				apObs = nil // closed: a nil channel is never selected again
				continue
			}
			diag.Observe(o)
		case b, ok := <-tel:
			if !ok {
				tel = nil
				continue
			}
			if discontinuity(last, b.At) {
				diag.Reset()
				mgr.Reset(b.At)
			}
			last = b.At
			statuses := diag.Process(b)
			e.store.PutStatuses(statuses)
			change := mgr.Process(statuses, b.At)
			if truth := e.sim.Truth(); truth != nil && truth.Seq == b.Seq {
				e.eval.ScoreDiagnostics(statuses, truth, e.cfg.Thresholds.DeadZoneMinDevs, e.cfg.Thresholds.FailureTimeout)
			}
			e.Updates.Publish(campus.Envelope{Type: "wifi", At: b.At, Data: statuses})
			for _, a := range change.Raised {
				e.log.Info("alert raised", "id", a.ID, "kind", a.Kind, "title", a.Title)
				e.Updates.Publish(campus.Envelope{Type: "alert", At: b.At, Data: a})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Frames
// ---------------------------------------------------------------------------

func (e *Engine) frameLoop(in <-chan campus.SimFrame) {
	var lastReal time.Time
	for f := range in {
		now := time.Now()
		if !lastReal.IsZero() && f.Observed > 0 {
			rate := float64(f.Observed) / now.Sub(lastReal).Seconds()
			e.obsRate.Store(math.Float64bits(rate))
		}
		lastReal = now
		f.Flows = CoarsenFlows(f.Flows)
		e.store.PutFrame(f)
		e.Updates.Publish(campus.Envelope{Type: "frame", At: f.At, Data: f})
	}
}

// CoarsenFlows rounds movement flows to multiples of 5 and drops tiny ones,
// so the 3D animation shows crowds moving without revealing individuals.
func CoarsenFlows(fs []campus.MovementFlow) []campus.MovementFlow {
	out := fs[:0:0]
	for _, f := range fs {
		f.Count = campus.OccupancyCount(math.Round(float64(f.Count)/5) * 5)
		if f.Count > 0 {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Forecasts
// ---------------------------------------------------------------------------

func (e *Engine) forecastLoop(ctx context.Context, ticks <-chan tickNote) {
	pool, err := workerpool.New("forecast", max(e.cfg.ForecastWorkers, 1), func(ctx context.Context, req campus.ForecastRequest) (campus.Forecast, error) {
		return e.fc.Forecast(ctx, req)
	})
	if err != nil {
		e.log.Error("forecast pool", "err", err)
		for range ticks {
		}
		return
	}
	for t := range ticks {
		reqs := []campus.ForecastRequest{{Subject: "campus", Kind: "campus"}}
		for _, b := range e.campus.Buildings {
			reqs = append(reqs, campus.ForecastRequest{Subject: string(b.ID), Kind: "building"})
			for _, f := range b.Floors {
				reqs = append(reqs, campus.ForecastRequest{Subject: string(f.ID), Kind: "floor"})
			}
		}
		in := make(chan campus.ForecastRequest, len(reqs))
		for _, r := range reqs {
			in <- r
		}
		close(in)
		var results []campus.Forecast
		for res := range pool.Run(ctx, in) { // fan-out over workers, fan-in here
			if res.Err != nil {
				e.log.Debug("forecast failed", "err", res.Err)
				continue
			}
			results = append(results, res.Value)
		}
		e.store.PutForecasts(results)

		people, _ := e.store.People()
		warnings := forecast.ChangeoverWarnings(e.campus, e.sim.Schedule(), t.at, 3*time.Hour,
			func(id campus.RoomID) float64 { return people[id] }, e.fc.AttendanceRatio(), e.cfg.ChangeoverMin)
		e.store.PutWarnings(warnings)
		e.Updates.Publish(campus.Envelope{Type: "forecast", At: t.at, Data: ForecastMessage{Forecasts: results, Warnings: warnings}})
	}
}

// ForecastMessage is the WebSocket payload for forecast updates.
type ForecastMessage struct {
	Forecasts []campus.Forecast            `json:"forecasts"`
	Warnings  []forecast.ChangeoverWarning `json:"warnings"`
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

func (e *Engine) historyLoop(ticks <-chan tickNote) {
	for t := range ticks {
		v := map[string]float64{}
		if g, err := e.store.Group("campus"); err == nil {
			v["campus.occupancy"] = float64(g.Count.Value)
		}
		for _, b := range e.campus.Buildings {
			if g, err := e.store.Group(string(b.ID)); err == nil {
				v["building."+string(b.ID)+".occupancy"] = float64(g.Count.Value)
				v["building."+string(b.ID)+".ratio"] = g.Ratio
			}
		}
		var lat, util float64
		var online, problems int
		for _, st := range e.store.Statuses() {
			if st.Health == campus.APOffline {
				problems++
				continue
			}
			online++
			v["ap."+string(st.AP)+".latency"] = float64(st.Telemetry.Latency)
			v["ap."+string(st.AP)+".utilization"] = float64(st.Telemetry.Utilization)
			lat += float64(st.Telemetry.Latency)
			util += float64(st.Telemetry.Utilization)
			if st.Health != campus.APHealthy {
				problems++
			}
		}
		if online > 0 {
			v["wifi.avg_latency"] = lat / float64(online)
			v["wifi.avg_utilization"] = util / float64(online)
		}
		v["wifi.online_aps"] = float64(online)
		v["wifi.problem_aps"] = float64(problems)
		v["alerts.active"] = float64(len(e.store.Alerts(true)))
		if f, ok := e.store.Forecast("campus", "campus"); ok && len(f.Points) > 2 {
			v["forecast.campus.60"] = float64(f.Points[2].Count)
		}
		e.store.Record(t.at, v)
	}
}
