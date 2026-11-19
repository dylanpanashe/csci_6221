// Package forecast predicts occupancy 15, 30, 60 and 120 minutes ahead.
//
// The default model is rule-based and fully explainable. For a room at time
// T = now + horizon:
//
//   - If a class will be running at T, the target is its expected
//     attendance scaled by the attendance ratio the system has learned by
//     comparing Wi-Fi estimates with the timetable today.
//   - If nothing is scheduled at T, the target is the room's learned
//     pattern for that time of day (seeded with a prior per room type).
//   - Lobbies add a changeover surge when classes on their floor or in their
//     building end or start near T.
//   - The forecast blends the current estimate with that target. The weight
//     on "now" decays with the horizon, and drops to zero if the class that
//     explains the current crowd ends before T.
//
// Each forecast lists the main factors behind it in plain language.
package forecast

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/fn"
)

// Horizons are the standard forecast horizons.
var Horizons = []time.Duration{15 * time.Minute, 30 * time.Minute, 60 * time.Minute, 120 * time.Minute}

// Snapshot is the current state a forecast starts from.
type Snapshot struct {
	At        time.Time
	People    map[campus.RoomID]float64 // current raw estimates (internal only)
	Timetable campus.Timetable
}

// Provider supplies snapshots. The engine injects one backed by the store.
type Provider interface {
	Snapshot() (Snapshot, error)
}

// ProviderFunc lets a plain function satisfy Provider (like http.HandlerFunc).
type ProviderFunc func() (Snapshot, error)

func (f ProviderFunc) Snapshot() (Snapshot, error) { return f() }

// RoomInput is everything a strategy needs for one room.
type RoomInput struct {
	Room      *campus.Room
	Campus    *campus.Campus
	Now       time.Time
	Current   float64
	Timetable campus.Timetable
	Baseline  func(campus.RoomID, time.Time) float64 // learned pattern
	Ratio     float64                                // learned attendance ratio
}

// Strategy predicts one room at one horizon. Strategies are plain function
// values, so a new model is just a new function in the registry.
type Strategy func(in RoomInput, horizon time.Duration) campus.ForecastPoint

// Params are the tunable constants of the rule-based model.
type Params struct {
	FreeTau    time.Duration // how fast "now" fades toward the pattern when no class is involved
	SurgeScale float64       // multiplier on the lobby changeover surge
}

// DefaultParams were picked by replaying recorded simulated days; see
// docs/DATA_PIPELINE.md.
var DefaultParams = Params{FreeTau: 45 * time.Minute, SurgeScale: 1.5}

// Strategies is the registry of available models.
var Strategies = map[string]Strategy{
	"rule_based":  NewRuleBased(DefaultParams),
	"persistence": Persistence,
}

// Persistence is the naive baseline: "it will stay as it is". It exists so
// the evaluator can show how much the rule-based model actually adds.
func Persistence(in RoomInput, h time.Duration) campus.ForecastPoint {
	return point(in, h, in.Current, 0.9-0.002*h.Minutes(), []string{"Assumes current level continues"})
}

// NewRuleBased returns the default model described in the package comment,
// configured with p. The returned function is a closure over p.
func NewRuleBased(p Params) Strategy {
	return func(in RoomInput, h time.Duration) campus.ForecastPoint { return ruleBased(in, h, p) }
}

func ruleBased(in RoomInput, h time.Duration, p Params) campus.ForecastPoint {
	t := in.Now.Add(h)
	var factors []string
	var target float64
	persist := math.Exp(-h.Minutes() / p.FreeTau.Minutes())

	running := activeEvent(in.Timetable, in.Room.ID, in.Now)
	future := activeEvent(in.Timetable, in.Room.ID, t)
	switch {
	case future != nil && running != nil && future.ID == running.ID:
		target = in.Current
		persist = 0.9
		factors = append(factors, fmt.Sprintf("%s continues until %s", future.Title, clock(future.Window.End)))
	case future != nil:
		target = float64(future.ExpectedAttendance) * in.Ratio
		persist = 0
		factors = append(factors, fmt.Sprintf("%s scheduled %s–%s (about %d expected)", future.Title,
			clock(future.Window.Start), clock(future.Window.End), int(target)))
	default:
		target = in.Baseline(in.Room.ID, t)
		factors = append(factors, fmt.Sprintf("No class scheduled; typical level for %s", clock(t)))
		if running != nil && running.Window.End.Before(t) {
			persist = 0
			factors = append(factors, fmt.Sprintf("%s ends at %s", running.Title, clock(running.Window.End)))
		}
	}

	if in.Room.IsCirculation() {
		surge, why := lobbySurge(in, t)
		target += surge * p.SurgeScale
		factors = append(factors, why...)
	}
	if persist > 0.3 && in.Current > 0 {
		factors = append(factors, "Current occupancy carried forward")
	}
	value := persist*in.Current + (1-persist)*target
	conf := 0.92 - 0.0025*h.Minutes()
	return point(in, h, value, conf, factors)
}

// lobbySurge estimates extra people in a lobby around time t from classes
// ending (people leaving) and starting (people arriving) nearby.
func lobbySurge(in RoomInput, t time.Time) (float64, []string) {
	var surge float64
	var why []string
	for _, e := range in.Timetable.Between(t.Add(-20*time.Minute), t.Add(25*time.Minute)) {
		if e.Cancelled || e.BuildingID != in.Room.BuildingID {
			continue
		}
		share := 0.0
		switch {
		case e.Level == in.Room.Level && in.Room.Level == 1:
			share = 0.55
		case e.Level == in.Room.Level:
			share = 0.35
		case in.Room.Level == 1:
			share = 0.3 // everyone leaving the building passes the ground floor
		}
		if share == 0 {
			continue
		}
		people := float64(e.ExpectedAttendance) * in.Ratio
		if d := t.Sub(e.Window.End); d >= -2*time.Minute && d <= 15*time.Minute && people >= 20 {
			surge += share * people
			why = append(why, fmt.Sprintf("%s lets out at %s (~%d people)", e.Title, clock(e.Window.End), int(people)))
		}
		// Students wait in the floor lobby for up to 20 minutes before class.
		if d := e.Window.Start.Sub(t); d > 0 && d <= 20*time.Minute && people >= 20 {
			wait := 0.25
			if e.Level == in.Room.Level {
				wait = 0.5
			}
			surge += wait * people
			why = append(why, fmt.Sprintf("Students gathering for %s at %s", e.Title, clock(e.Window.Start)))
		}
	}
	return surge, why
}

func point(in RoomInput, h time.Duration, value, conf float64, factors []string) campus.ForecastPoint {
	value = math.Max(0, value)
	ratio := 0.0
	if in.Room.Capacity > 0 {
		ratio = value / float64(in.Room.Capacity)
	}
	return campus.ForecastPoint{
		Horizon: h, Minutes: int(h.Minutes()), At: in.Now.Add(h),
		Count: campus.OccupancyCount(math.Round(value)), Ratio: ratio, Level: campus.LevelForRatio(ratio),
		Confidence: fn.Clamp(conf, 0.3, 0.95), Factors: factors,
	}
}

func activeEvent(tt campus.Timetable, room campus.RoomID, t time.Time) *campus.TimetableEvent {
	evs := tt.EventsAt(room, t)
	if len(evs) == 0 {
		return nil
	}
	return &evs[0]
}

func clock(t time.Time) string { return t.Format("3:04 PM") }

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// Compile-time interface check.
var _ campus.ForecastEngine = (*Engine)(nil)

// Engine implements campus.ForecastEngine. It also learns two things as
// estimates arrive: a time-of-day pattern per room and the ratio between
// observed and timetabled attendance.
type Engine struct {
	campus   *campus.Campus
	provider Provider
	strategy Strategy

	mu      sync.RWMutex // guards the learned state below
	profile map[campus.RoomID]*[48]float64
	seen    map[campus.RoomID]*[48]bool
	scale   map[campus.RoomID]float64 // how busy this room runs compared with its prior
	ratio   float64
}

// NewEngine creates a forecaster using the named strategy.
func NewEngine(c *campus.Campus, p Provider, strategy string) (*Engine, error) {
	s, ok := Strategies[strategy]
	if !ok {
		return nil, &campus.ValidationError{Field: "strategy", Value: strategy, Reason: "unknown forecast strategy"}
	}
	e := &Engine{campus: c, provider: p, strategy: s, ratio: 1,
		profile: map[campus.RoomID]*[48]float64{}, seen: map[campus.RoomID]*[48]bool{}, scale: map[campus.RoomID]float64{}}
	for _, r := range c.AllRooms() {
		var p [48]float64
		for b := range p {
			p[b] = prior(r, b)
		}
		e.profile[r.ID] = &p
		e.seen[r.ID] = &[48]bool{}
		e.scale[r.ID] = 1
	}
	return e, nil
}

func bellAt(hour float64) float64 { return math.Exp(-math.Pow((hour-13.5)/3.5, 2)) }

// prior is the starting time-of-day pattern for a room type, as a share of
// capacity in each half-hour bucket.
func prior(r *campus.Room, bucket int) float64 {
	return priorShare(r, bellAt(float64(bucket)/2))
}

// priorShare maps a 0..1 campus activity level to an expected headcount.
func priorShare(r *campus.Room, activity float64) float64 {
	bell := activity
	share := 0.0
	switch r.Type {
	case campus.RoomLibraryArea:
		share = 0.15 + 0.45*bell
	case campus.RoomStudyRoom:
		share = 0.1 + 0.5*bell
	case campus.RoomLobby:
		share = 0.02 + 0.05*bell
	default:
		share = 0.03 * bell // empty classrooms and labs get the odd student
	}
	return share * float64(r.Capacity)
}

func bucketOf(t time.Time) int { return t.Hour()*2 + t.Minute()/30 }

// Learn updates the pattern and attendance ratio from a tick of raw
// estimates. Called by the fusion goroutine.
func (e *Engine) Learn(at time.Time, people map[campus.RoomID]float64, tt campus.Timetable) {
	b := bucketOf(at)
	var obs, exp float64
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, n := range people {
		evs := tt.EventsAt(id, at)
		if len(evs) > 0 {
			// Only learn the ratio from classes past their first 10 minutes.
			if at.Sub(evs[0].Window.Start) > 10*time.Minute {
				obs += n
				exp += float64(evs[0].ExpectedAttendance)
			}
			continue // do not teach the free-time pattern from class time
		}
		p, ok := e.profile[id]
		if !ok {
			continue
		}
		// Track how this room compares with its prior right now, so
		// unseen times later today are scaled to the room's real level.
		if r, err := e.campus.Room(id); err == nil {
			if pr := prior(r, b); pr >= 0.5 {
				k := min(max(n/pr, 0.2), 6)
				e.scale[id] = 0.8*e.scale[id] + 0.2*k
			}
		}
		if !e.seen[id][b] {
			p[b] = 0.5*p[b] + 0.5*n
			e.seen[id][b] = true
		} else {
			p[b] = 0.85*p[b] + 0.15*n
		}
	}
	if exp > 50 {
		e.ratio = 0.9*e.ratio + 0.1*(obs/exp)
	}
}

// AttendanceRatio returns the learned observed/expected attendance ratio.
func (e *Engine) AttendanceRatio() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ratio
}

// baselineFor returns a pattern lookup bound to one timetable.
func (e *Engine) baselineFor(tt campus.Timetable) func(campus.RoomID, time.Time) float64 {
	return func(id campus.RoomID, t time.Time) float64 {
		r, err := e.campus.Room(id)
		if err != nil {
			return 0
		}
		e.mu.RLock()
		defer e.mu.RUnlock()
		b := bucketOf(t)
		if e.seen[id][b] {
			return e.profile[id][b] // observed at this time of day before: trust history
		}
		return prior(r, b) * e.scale[id]
	}
}

// RoomPoints forecasts one room at the given horizons.
func (e *Engine) RoomPoints(s Snapshot, r *campus.Room, horizons []time.Duration) []campus.ForecastPoint {
	in := RoomInput{Room: r, Campus: e.campus, Now: s.At, Current: s.People[r.ID], Timetable: s.Timetable,
		Baseline: e.baselineFor(s.Timetable), Ratio: e.AttendanceRatio()}
	return fn.Map(horizons, func(h time.Duration) campus.ForecastPoint { return e.strategy(in, h) })
}

// Predict forecasts every room at one horizon with a named strategy and
// returns raw values. The evaluator uses it to score models side by side.
func (e *Engine) Predict(s Snapshot, h time.Duration, strategy string) map[campus.RoomID]float64 {
	strat, ok := Strategies[strategy]
	if !ok {
		strat = e.strategy
	}
	ratio := e.AttendanceRatio()
	out := make(map[campus.RoomID]float64, len(s.People))
	for _, r := range e.campus.AllRooms() {
		in := RoomInput{Room: r, Campus: e.campus, Now: s.At, Current: s.People[r.ID], Timetable: s.Timetable,
			Baseline: e.baselineFor(s.Timetable), Ratio: ratio}
		out[r.ID] = float64(strat(in, h).Count)
	}
	return out
}

// Forecast implements campus.ForecastEngine for a room, floor or building.
func (e *Engine) Forecast(ctx context.Context, req campus.ForecastRequest) (campus.Forecast, error) {
	if err := ctx.Err(); err != nil {
		return campus.Forecast{}, err
	}
	horizons := req.Horizons
	if len(horizons) == 0 {
		horizons = Horizons
	}
	rooms, err := e.roomsFor(req)
	if err != nil {
		return campus.Forecast{}, err
	}
	snap, err := e.provider.Snapshot()
	if err != nil {
		return campus.Forecast{}, fmt.Errorf("forecast: snapshot: %w", err)
	}
	f := campus.Forecast{Subject: req.Subject, Kind: req.Kind, At: snap.At}
	if len(rooms) == 1 {
		f.Points = e.RoomPoints(snap, rooms[0], horizons)
		return f, nil
	}
	// Sum room forecasts into a group forecast and keep the strongest reasons.
	var capacity float64
	f.Points = make([]campus.ForecastPoint, len(horizons))
	weights := make([]map[string]float64, len(horizons))
	for i := range weights {
		weights[i] = map[string]float64{}
	}
	for _, r := range rooms {
		capacity += float64(r.Capacity)
		for i, p := range e.RoomPoints(snap, r, horizons) {
			agg := &f.Points[i]
			agg.Horizon, agg.Minutes, agg.At = p.Horizon, p.Minutes, p.At
			agg.Count += p.Count
			agg.Confidence += p.Confidence * float64(p.Count+1)
			for _, why := range p.Factors {
				weights[i][why] += float64(p.Count + 1)
			}
		}
	}
	for i := range f.Points {
		p := &f.Points[i]
		total := float64(p.Count) + float64(len(rooms))
		p.Confidence /= total
		if capacity > 0 {
			p.Ratio = float64(p.Count) / capacity
		}
		p.Level = campus.LevelForRatio(p.Ratio)
		p.Factors = topFactors(weights[i], 3)
	}
	return f, nil
}

func topFactors(w map[string]float64, n int) []string {
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if w[keys[i]] != w[keys[j]] {
			return w[keys[i]] > w[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return fn.Take(keys, n)
}

func (e *Engine) roomsFor(req campus.ForecastRequest) ([]*campus.Room, error) {
	switch req.Kind {
	case "room":
		r, err := e.campus.Room(campus.RoomID(req.Subject))
		if err != nil {
			return nil, err
		}
		return []*campus.Room{r}, nil
	case "floor":
		f, err := e.campus.FloorByID(campus.FloorID(req.Subject))
		if err != nil {
			return nil, err
		}
		return append(append([]*campus.Room(nil), f.Rooms...), f.Lobby), nil
	case "building":
		b, err := e.campus.Building(campus.BuildingID(req.Subject))
		if err != nil {
			return nil, err
		}
		var rs []*campus.Room
		for _, f := range b.Floors {
			rs = append(append(rs, f.Rooms...), f.Lobby)
		}
		return rs, nil
	case "campus", "":
		return e.campus.AllRooms(), nil
	default:
		return nil, &campus.ValidationError{Field: "kind", Value: req.Kind, Reason: "must be room, floor, building or campus"}
	}
}
