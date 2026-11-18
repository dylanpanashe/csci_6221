// Package app wires every component together and owns the application
// lifecycle: start the workers, serve HTTP, and shut everything down in
// order when the context is cancelled.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/config"
	"github.com/dylanpanashe/csci_6221/internal/simclock"
)

// App is the assembled application. Its fields are set once in New and then
// only read, so they need no locking.
type App struct {
	cfg    config.Config
	log    *slog.Logger
	campus *campus.Campus
	clock  *simclock.SimClock
	server *http.Server

	started time.Time
	wg      sync.WaitGroup // tracks background goroutines for clean shutdown
}

// New builds the application from configuration. It does not start anything.
func New(cfg config.Config, log *slog.Logger) (*App, error) {
	loc := cfg.Location()
	start := simclock.DayAt(time.Now(), loc, cfg.SimStartHour, 0)
	clk, err := simclock.New(start, cfg.SimSpeed, time.Now)
	if err != nil {
		return nil, fmt.Errorf("app: clock: %w", err)
	}

	a := &App{
		cfg:     cfg,
		log:     log,
		campus:  campus.NewGWUCampus(),
		clock:   clk,
		started: time.Now(),
	}
	a.server = &http.Server{
		Addr:              cfg.Addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return a, nil
}

// Run serves until ctx is cancelled, then shuts down gracefully. It returns
// nil on a clean shutdown.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return fmt.Errorf("app: listen on %s: %w", a.cfg.Addr, err)
	}
	a.log.Info("CampusPulse listening",
		"url", "http://localhost"+displayPort(ln.Addr()),
		"sim_clock", a.clock.String(),
		"spaces", len(a.campus.AllRooms()),
		"access_points", len(a.campus.AllAccessPoints()))

	serveErr := make(chan error, 1) // buffered: the goroutine never blocks on send
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if err := a.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		a.log.Info("shutdown requested", "cause", context.Cause(ctx))
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("app: serve: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("app: http shutdown: %w", err)
	}
	a.wg.Wait()
	a.log.Info("shutdown complete")
	return nil
}

// Handler exposes the HTTP handler for integration tests (httptest.Server).
func (a *App) Handler() http.Handler { return a.server.Handler }

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("GET /api/campus", a.handleCampus)
	mux.Handle("GET /", http.FileServer(http.Dir(a.cfg.WebDir)))
	return mux
}

func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"uptime":   time.Since(a.started).Round(time.Second).String(),
		"sim_time": a.clock.Now(),
	})
}

func (a *App) handleCampus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.campus)
}

// writeJSON is a temporary helper; Stage 4 replaces it with the API
// package's consistent response envelope.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers already sent; nothing useful to do on error
}

func displayPort(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok { // type assertion
		return fmt.Sprintf(":%d", tcp.Port)
	}
	return addr.String()
}
