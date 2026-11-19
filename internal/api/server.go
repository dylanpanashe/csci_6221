package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/accuracy"
	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
	"github.com/dylanpanashe/csci_6221/internal/forecast"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
	"github.com/dylanpanashe/csci_6221/internal/store"
)

// State is the read-only view of live data the API needs. *store.Store
// satisfies it. Nothing in it can return raw (pre-privacy) estimates or
// simulator ground truth, so no handler can leak them.
type State interface {
	campus.ReadRepository
	Group(id string) (campus.OccupancyEstimate, error)
	Statuses() []diagnostics.APStatus
	Status(id campus.AccessPointID) (diagnostics.APStatus, bool)
	Forecast(kind, subject string) (campus.Forecast, bool)
	Warnings() []forecast.ChangeoverWarning
	Series(names ...string) map[string][]store.Point
	Frame() campus.SimFrame
}

// Timetable is the timetable view the API reads.
type Timetable interface {
	campus.Timetable
	RoomEvents(room campus.RoomID) []campus.TimetableEvent
}

// Simulation is the set of simulator controls the API may call.
type Simulation interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Reset(ctx context.Context) error
	SetSpeed(ctx context.Context, x float64) error
	AdjustActivity(ctx context.Context, delta float64) error
	TriggerChangeover(ctx context.Context) error
	FailAP(ctx context.Context, id campus.AccessPointID) error
	RestoreAP(ctx context.Context, id campus.AccessPointID) error
	DemoScenario(ctx context.Context) error
	Status() simulator.Status
}

// Compile-time checks that the real implementations fit.
var (
	_ State      = (*store.Store)(nil)
	_ Timetable  = (*simulator.Schedule)(nil)
	_ Simulation = (*simulator.Simulator)(nil)
)

// Deps are the server's dependencies, injected by the app. Tests pass fakes.
type Deps struct {
	Campus      *campus.Campus
	State       State
	Forecaster  campus.ForecastEngine
	Timetable   func() Timetable
	Sim         Simulation
	Clock       campus.Clock
	Accuracy    func() accuracy.Report
	Metrics     func() map[string]any
	WebSocket   http.Handler
	WebDir      string
	EnablePprof bool
	CORSOrigins []string
	Log         *slog.Logger
	Started     time.Time
}

// Server holds the routes and handlers.
type Server struct {
	deps Deps
	log  *slog.Logger
}

// New creates a server.
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Server{deps: d, log: d.Log}
}

// Handler builds the routing table and wraps it in the middleware chain.
// Routes use Go 1.22 method and wildcard patterns, so no router library
// is needed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	routes := []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET /api/health", s.health},
		{"GET /api/campus", s.campusInfo},
		{"GET /api/buildings", s.buildings},
		{"GET /api/buildings/{id}", s.building},
		{"GET /api/buildings/{id}/floors", s.floors},
		{"GET /api/rooms", s.findRooms},
		{"GET /api/rooms/{id}", s.room},
		{"GET /api/recommendations/quiet", s.quietPlace},
		{"GET /api/wifi/access-points", s.accessPoints},
		{"GET /api/wifi/access-points/{id}", s.accessPoint},
		{"GET /api/wifi/dead-zones", s.deadZones},
		{"GET /api/alerts", s.alerts},
		{"GET /api/forecast", s.forecast},
		{"GET /api/changeovers", s.changeovers},
		{"GET /api/history", s.history},
		{"GET /api/metrics", s.metrics},
		{"GET /api/accuracy", s.accuracy},
		{"GET /api/simulation", s.simulation},
		{"POST /api/simulation/control", s.control},
	}
	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, rt.h)
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, &campus.NotFoundError{Kind: "endpoint", ID: r.Method + " " + r.URL.Path})
	})
	if s.deps.EnablePprof {
		// Development only: CPU, heap and goroutine profiles.
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	if s.deps.WebDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.deps.WebDir)))
	}

	quiet := func(r *http.Request) bool { return !strings.HasPrefix(r.URL.Path, "/api/") }
	app := Chain(
		RequestID(),
		Recover(s.log),
		Logging(s.log, quiet),
		Timing(),
		SecurityHeaders(),
		CORS(s.deps.CORSOrigins),
		ValidateJSON(1<<16),
	)(mux)

	// The WebSocket upgrade needs the raw http.Hijacker, which the
	// response-wrapping middleware above would hide, so /ws gets only the
	// middleware that leaves the writer alone.
	root := http.NewServeMux()
	if s.deps.WebSocket != nil {
		root.Handle("GET /ws", Chain(RequestID(), Recover(s.log))(s.deps.WebSocket))
	}
	root.Handle("/", app)
	return root
}
