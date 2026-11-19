// Package store holds the live state that the API and WebSocket hub read:
// estimates, AP status, alerts, forecasts and history.
//
// Concurrency: one sync.RWMutex guards everything. Writers are the engine's
// pipeline goroutines (a few writes every two seconds); readers are HTTP
// handlers (many). RWMutex lets readers proceed in parallel. Every read
// returns copies, never pointers into the maps, so a caller can hold a
// result as long as it likes without racing the next write.
package store

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
	"github.com/dylanpanashe/csci_6221/internal/forecast"
)

// Compile-time interface checks.
var (
	_ campus.Repository     = (*Store)(nil)
	_ campus.ReadRepository = (*Store)(nil)
)

// Point is one value in a time series.
type Point struct {
	At    time.Time `json:"t"`
	Value float64   `json:"v"`
}

// Store is the in-memory state store.
type Store struct {
	mu sync.RWMutex

	estimates map[campus.RoomID]campus.OccupancyEstimate // published (privacy-safe)
	groups    map[string]campus.OccupancyEstimate        // published floor/building/campus totals
	people    map[campus.RoomID]float64                  // raw, internal only
	estAt     time.Time

	telemetry map[campus.AccessPointID]campus.APTelemetry
	status    map[campus.AccessPointID]diagnostics.APStatus

	alerts    []campus.Alert
	open      map[alertKey]int // index into alerts for open alerts
	maxAlerts int

	forecasts map[string]campus.Forecast
	warnings  []forecast.ChangeoverWarning
	frame     campus.SimFrame

	series     map[string]*Ring[Point]
	seriesSize int
}

type alertKey struct {
	ap   campus.AccessPointID
	kind campus.DiagnosisKind
}

// New creates an empty store keeping seriesSize points per time series.
func New(seriesSize int) *Store {
	if seriesSize < 1 {
		seriesSize = 240
	}
	return &Store{
		estimates:  map[campus.RoomID]campus.OccupancyEstimate{},
		people:     map[campus.RoomID]float64{},
		telemetry:  map[campus.AccessPointID]campus.APTelemetry{},
		status:     map[campus.AccessPointID]diagnostics.APStatus{},
		open:       map[alertKey]int{},
		maxAlerts:  500,
		forecasts:  map[string]campus.Forecast{},
		series:     map[string]*Ring[Point]{},
		seriesSize: seriesSize,
	}
}

// ---------------------------------------------------------------------------
// Occupancy
// ---------------------------------------------------------------------------

// PutEstimates implements campus.OccupancyWriter (published estimates).
func (s *Store) PutEstimates(es []campus.OccupancyEstimate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range es {
		s.estimates[e.RoomID] = e
		if e.At.After(s.estAt) {
			s.estAt = e.At
		}
	}
}

// PutGroups stores published floor, building and campus totals.
func (s *Store) PutGroups(g map[string]campus.OccupancyEstimate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups = maps.Clone(g)
}

// Group returns a published total by ID ("campus", "bell", "bell-2", ...).
func (s *Store) Group(id string) (campus.OccupancyEstimate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return campus.OccupancyEstimate{}, &campus.NotFoundError{Kind: "occupancy group", ID: id}
	}
	return g, nil
}

// PutPeople stores raw estimates for internal consumers (the forecaster).
// It is deliberately not part of any interface the API layer receives.
func (s *Store) PutPeople(at time.Time, people map[campus.RoomID]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.people = maps.Clone(people)
	s.estAt = at
}

// People returns a copy of the raw estimates and their time.
func (s *Store) People() (map[campus.RoomID]float64, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.people), s.estAt
}

// RoomEstimate implements campus.OccupancyReader.
func (s *Store) RoomEstimate(id campus.RoomID) (campus.OccupancyEstimate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.estimates[id]
	if !ok {
		return campus.OccupancyEstimate{}, &campus.NotFoundError{Kind: "estimate", ID: string(id)}
	}
	return e, nil
}

// RoomEstimates implements campus.OccupancyReader.
func (s *Store) RoomEstimates() []campus.OccupancyEstimate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Collect(maps.Values(s.estimates))
	slices.SortFunc(out, func(a, b campus.OccupancyEstimate) int { return compareStr(string(a.RoomID), string(b.RoomID)) })
	return out
}

// ---------------------------------------------------------------------------
// Wi-Fi
// ---------------------------------------------------------------------------

// PutTelemetry implements campus.TelemetryWriter.
func (s *Store) PutTelemetry(t campus.APTelemetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.telemetry[t.AccessPoint] = t
}

// Telemetry implements campus.TelemetryReader.
func (s *Store) Telemetry(id campus.AccessPointID) (campus.APTelemetry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.telemetry[id]
	if !ok {
		return campus.APTelemetry{}, &campus.NotFoundError{Kind: "telemetry", ID: string(id)}
	}
	return t, nil
}

// AllTelemetry implements campus.TelemetryReader.
func (s *Store) AllTelemetry() []campus.APTelemetry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Collect(maps.Values(s.telemetry))
	slices.SortFunc(out, func(a, b campus.APTelemetry) int { return compareStr(string(a.AccessPoint), string(b.AccessPoint)) })
	return out
}

// PutStatuses stores the latest diagnostic status for every AP.
func (s *Store) PutStatuses(st []diagnostics.APStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range st {
		s.status[x.AP] = x
		if x.Telemetry.Online {
			s.telemetry[x.AP] = x.Telemetry
		}
	}
}

// Status returns one AP's diagnostic status.
func (s *Store) Status(id campus.AccessPointID) (diagnostics.APStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	x, ok := s.status[id]
	return x, ok
}

// Statuses returns every AP status sorted by AP ID.
func (s *Store) Statuses() []diagnostics.APStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Collect(maps.Values(s.status))
	slices.SortFunc(out, func(a, b diagnostics.APStatus) int { return compareStr(string(a.AP), string(b.AP)) })
	return out
}

// APOffline reports whether an AP is currently diagnosed as failed.
func (s *Store) APOffline(id campus.AccessPointID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status[id].Health == campus.APOffline
}

// ---------------------------------------------------------------------------
// Alerts
// ---------------------------------------------------------------------------

// RaiseAlert implements campus.AlertWriter. It returns false if the same
// problem on the same AP already has an open alert.
func (s *Store) RaiseAlert(a campus.Alert) (campus.Alert, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := alertKey{a.AccessPoint, a.Kind}
	if i, ok := s.open[k]; ok {
		// Keep the original alert but refresh its detail text.
		s.alerts[i].Detail = a.Detail
		if a.Severity > s.alerts[i].Severity {
			s.alerts[i].Severity = a.Severity
		}
		return s.alerts[i], false
	}
	s.alerts = append(s.alerts, a)
	s.open[k] = len(s.alerts) - 1
	s.trimLocked()
	return a, true
}

// ResolveAlerts implements campus.AlertWriter.
func (s *Store) ResolveAlerts(ap campus.AccessPointID, kind campus.DiagnosisKind, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := alertKey{ap, kind}
	i, ok := s.open[k]
	if !ok {
		return 0
	}
	s.alerts[i].Resolve(at)
	delete(s.open, k)
	return 1
}

// trimLocked drops the oldest resolved alerts once the history is full.
// Caller must hold the write lock.
func (s *Store) trimLocked() {
	if len(s.alerts) <= s.maxAlerts {
		return
	}
	kept := s.alerts[:0:0]
	drop := len(s.alerts) - s.maxAlerts
	for _, a := range s.alerts {
		if drop > 0 && !a.Active() {
			drop--
			continue
		}
		kept = append(kept, a)
	}
	s.alerts = kept
	clear(s.open)
	for i, a := range s.alerts {
		if a.Active() {
			s.open[alertKey{a.AccessPoint, a.Kind}] = i
		}
	}
}

// Alerts implements campus.AlertReader, newest first.
func (s *Store) Alerts(activeOnly bool) []campus.Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]campus.Alert, 0, len(s.alerts))
	for i := len(s.alerts) - 1; i >= 0; i-- {
		if a := s.alerts[i]; !activeOnly || a.Active() {
			out = append(out, a)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Forecasts, warnings, frames
// ---------------------------------------------------------------------------

// PutForecasts replaces the precomputed forecasts.
func (s *Store) PutForecasts(fs []campus.Forecast) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range fs {
		s.forecasts[f.Kind+":"+f.Subject] = f
	}
}

// Forecast returns a precomputed forecast.
func (s *Store) Forecast(kind, subject string) (campus.Forecast, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.forecasts[kind+":"+subject]
	return f, ok
}

// PutWarnings replaces the changeover warnings.
func (s *Store) PutWarnings(w []forecast.ChangeoverWarning) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warnings = slices.Clone(w)
}

// Warnings returns the current changeover warnings.
func (s *Store) Warnings() []forecast.ChangeoverWarning {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.warnings)
}

// PutFrame stores the latest simulator frame.
func (s *Store) PutFrame(f campus.SimFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frame = f
}

// Frame returns the latest simulator frame.
func (s *Store) Frame() campus.SimFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.frame
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

// Record appends points to named series in one locked section.
func (s *Store) Record(at time.Time, values map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, v := range values {
		r := s.series[name]
		if r == nil {
			r = NewRing[Point](s.seriesSize)
			s.series[name] = r
		}
		r.Push(Point{At: at, Value: v})
	}
}

// Series returns a copy of the named series, oldest first.
func (s *Store) Series(names ...string) map[string][]Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]Point, len(names))
	if len(names) == 0 {
		for name, r := range s.series {
			out[name] = r.Slice()
		}
		return out
	}
	for _, n := range names {
		if r, ok := s.series[n]; ok {
			out[n] = r.Slice()
		}
	}
	return out
}

// ResetHistory clears all series (after a simulation reset).
func (s *Store) ResetHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.series)
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
