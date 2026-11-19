// Package app wires every component together and owns the application
// lifecycle: start the pipeline, engine and WebSocket hub, serve HTTP, and
// shut everything down in order when the context is cancelled.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/api"
	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/config"
	"github.com/dylanpanashe/csci_6221/internal/engine"
	"github.com/dylanpanashe/csci_6221/internal/pipeline"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
	"github.com/dylanpanashe/csci_6221/internal/simclock"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
	"github.com/dylanpanashe/csci_6221/internal/store"
	"github.com/dylanpanashe/csci_6221/internal/websocket"
)

// App is the assembled application. Its fields are set once in New and then
// only read, so they need no locking.
type App struct {
	cfg    config.Config
	log    *slog.Logger
	campus *campus.Campus
	clock  *simclock.SimClock
	sim    *simulator.Simulator
	pipe   *pipeline.Pipeline
	store  *store.Store
	engine *engine.Engine
	hub    *websocket.Hub
	server *http.Server

	started time.Time
	wg      sync.WaitGroup // tracks background goroutines for clean shutdown
}

// New builds the application from configuration. It does not start anything.
func New(cfg config.Config, log *slog.Logger) (*App, error) {
	loc := cfg.Location()
	clk, err := simclock.New(simclock.DayAt(time.Now(), loc, cfg.SimStartHour, 0), cfg.SimSpeed, time.Now)
	if err != nil {
		return nil, fmt.Errorf("app: clock: %w", err)
	}
	c := campus.NewGWUCampus()
	sim, err := simulator.New(simulator.Config{
		Population: cfg.Population, Seed: uint64(cfg.Seed), Shards: 4, ChunkSize: 512,
		Tick: cfg.TickInterval, StartHour: cfg.SimStartHour, Location: loc, Demo: cfg.DemoScenario,
	}, c, clk, log.With("component", "simulator"))
	if err != nil {
		return nil, fmt.Errorf("app: simulator: %w", err)
	}
	hasher, err := privacy.NewHasher(cfg.HashSecret, loc)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	pipe, err := pipeline.New(pipeline.Config{Workers: cfg.Workers, Buffer: 64}, c, hasher, log.With("component", "pipeline"))
	if err != nil {
		return nil, fmt.Errorf("app: pipeline: %w", err)
	}
	st := store.New(300)
	ecfg := engine.DefaultConfig()
	ecfg.DevicesPerUser = cfg.DevicesPerUser
	ecfg.SuppressBelow = cfg.SuppressBelow
	ecfg.Noise = cfg.NoiseStdDev
	ecfg.Seed = uint64(cfg.Seed)
	eng, err := engine.New(ecfg, c, sim, st, log.With("component", "engine"))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	a := &App{cfg: cfg, log: log, campus: c, clock: clk, sim: sim, pipe: pipe, store: st, engine: eng, started: time.Now()}
	a.hub = websocket.NewHub(websocket.DefaultConfig(), log.With("component", "websocket"), a.snapshot)

	srv := api.New(api.Deps{
		Campus:      c,
		State:       st,
		Forecaster:  eng.Forecaster(),
		Timetable:   func() api.Timetable { return sim.Schedule() },
		Sim:         sim,
		Clock:       clk,
		Accuracy:    eng.Accuracy,
		Metrics:     a.metrics,
		WebSocket:   a.hub,
		WebDir:      cfg.WebDir,
		EnablePprof: cfg.EnablePprof,
		Log:         log.With("component", "api"),
		Started:     a.started,
	})
	a.server = &http.Server{Addr: cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	return a, nil
}

// Run serves until ctx is cancelled, then shuts down gracefully:
// stop the simulator → pipeline and engine drain → hub closes clients →
// HTTP server stops. It returns nil on a clean shutdown.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return fmt.Errorf("app: listen on %s: %w", a.cfg.Addr, err)
	}
	a.log.Info("CampusPulse listening",
		"url", "http://localhost"+displayPort(ln.Addr()),
		"sim_clock", a.clock.String(),
		"spaces", len(a.campus.AllRooms()),
		"access_points", len(a.campus.AllAccessPoints()),
		"pprof", a.cfg.EnablePprof)

	pipeCtx, stopPipe := context.WithCancel(ctx)
	defer stopPipe()
	// Subscribe before the engine starts so no early update is missed.
	updates, unsubscribe := a.engine.Updates.Subscribe(256)
	defer unsubscribe()

	a.wg.Add(2)
	go func() {
		defer a.wg.Done()
		a.engine.Run(pipeCtx, a.pipe.Start(pipeCtx, a.sim))
	}()
	go func() {
		defer a.wg.Done()
		a.hub.Run(pipeCtx, updates)
	}()

	serveErr := make(chan error, 1) // buffered: the goroutine never blocks on send
	go func() {
		if err := a.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown requested", "cause", context.Cause(ctx))
	case err := <-serveErr:
		if err != nil {
			runErr = fmt.Errorf("app: serve: %w", err)
		}
	}

	stopPipe()
	a.wg.Wait() // engine and hub have finished; WebSocket clients are closed
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		return errors.Join(runErr, fmt.Errorf("app: http shutdown: %w", err))
	}
	a.log.Info("shutdown complete")
	return runErr
}

// Handler exposes the HTTP handler for tests.
func (a *App) Handler() http.Handler { return a.server.Handler }

// snapshot is what a new WebSocket client receives first.
func (a *App) snapshot() []campus.Envelope {
	now := a.clock.Now()
	var out []campus.Envelope
	if f := a.store.Frame(); f.Seq > 0 {
		out = append(out, campus.Envelope{Type: "frame", At: now, Data: f})
	}
	groups := map[string]campus.OccupancyEstimate{}
	for _, id := range a.groupIDs() {
		if g, err := a.store.Group(id); err == nil {
			groups[id] = g
		}
	}
	out = append(out,
		campus.Envelope{Type: "occupancy", At: now, Data: engine.OccupancyMessage{Rooms: a.store.RoomEstimates(), Groups: groups}},
		campus.Envelope{Type: "wifi", At: now, Data: a.store.Statuses()},
		campus.Envelope{Type: "alerts", At: now, Data: a.store.Alerts(true)},
	)
	var fcs []campus.Forecast
	for _, b := range a.campus.Buildings {
		if f, ok := a.store.Forecast("building", string(b.ID)); ok {
			fcs = append(fcs, f)
		}
		for _, fl := range b.Floors {
			if f, ok := a.store.Forecast("floor", string(fl.ID)); ok {
				fcs = append(fcs, f)
			}
		}
	}
	if f, ok := a.store.Forecast("campus", "campus"); ok {
		fcs = append(fcs, f)
	}
	out = append(out, campus.Envelope{Type: "forecast", At: now, Data: engine.ForecastMessage{Forecasts: fcs, Warnings: a.store.Warnings()}})
	return out
}

func (a *App) groupIDs() []string {
	ids := []string{"campus"}
	for _, b := range a.campus.Buildings {
		ids = append(ids, string(b.ID))
		for _, f := range b.Floors {
			ids = append(ids, string(f.ID))
		}
	}
	return ids
}

func (a *App) metrics() map[string]any {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return map[string]any{
		"simulator":        a.sim.Status(),
		"pipeline":         a.pipe.Stats(),
		"websocket":        a.hub.Stats(),
		"updates_dropped":  a.engine.Updates.Dropped(),
		"accuracy":         a.engine.Accuracy(),
		"attendance_ratio": a.engine.AttendanceRatio(),
		"runtime": map[string]any{
			"goroutines":  runtime.NumGoroutine(),
			"heap_mb":     float64(ms.HeapAlloc) / (1 << 20),
			"sys_mb":      float64(ms.Sys) / (1 << 20),
			"gc_cycles":   ms.NumGC,
			"gc_pause_ms": float64(ms.PauseTotalNs) / 1e6,
			"cpus":        runtime.NumCPU(),
			"go_version":  runtime.Version(),
		},
	}
}

func displayPort(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok { // type assertion
		return fmt.Sprintf(":%d", tcp.Port)
	}
	return addr.String()
}
