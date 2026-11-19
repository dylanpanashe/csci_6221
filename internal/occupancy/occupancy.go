// Package occupancy turns de-duplicated device counts and the timetable into
// people estimates.
//
// The model, in plain terms:
//
//  1. Wi-Fi estimate = devices seen in the room ÷ calibration factor
//     (observed devices per person, default 1.65).
//  2. Timetable estimate = expected attendance of any class running now.
//  3. While a class is running, blend the two: mostly Wi-Fi, because the
//     timetable cannot see absences or cancellations. With no class, use
//     Wi-Fi alone. If the room's AP is down, fall back to the timetable.
//  4. Smooth over ticks so one noisy reading does not make a room flicker.
//  5. Report a range and a confidence, never a single "exact" number.
//
// Every calculation is a pure function. The only state, the smoothing
// memory, lives in Fusion and is owned by a single goroutine.
package occupancy

import (
	"math"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/fn"
)

// Compile-time interface check.
var _ campus.OccupancyEstimator = (*Fusion)(nil)

// Weights is the configurable part of the fusion model.
type Weights struct {
	WiFiDuringClass float64 // weight on Wi-Fi when a class is running (0..1)
	Smoothing       float64 // EWMA factor for new readings (1 = no smoothing)
}

// DefaultWeights were chosen by replaying a recorded simulated day (seed 7)
// through a grid of settings and checking room-level error against ground
// truth, then confirming on a different seed. Heavy smoothing made things
// worse: each tick is two simulated minutes, so averaging lags behind
// people actually moving.
var DefaultWeights = Weights{WiFiDuringClass: 0.75, Smoothing: 0.85}

// EstimateDevicePopulation converts a device count into people.
func EstimateDevicePopulation(devices campus.DeviceCount, devicesPerPerson float64) float64 {
	if devicesPerPerson <= 0 {
		return 0
	}
	return float64(devices) / devicesPerPerson
}

// ExpectedFromTimetable sums the expected attendance of running events.
func ExpectedFromTimetable(events []campus.TimetableEvent, at time.Time) float64 {
	return fn.SumBy(events, func(e campus.TimetableEvent) float64 {
		if !e.ActiveAt(at) {
			return 0
		}
		return float64(e.ExpectedAttendance)
	})
}

// FusionInput carries everything CalculateOccupancy needs.
type FusionInput struct {
	WiFiPeople   float64 // from EstimateDevicePopulation
	Expected     float64 // from ExpectedFromTimetable
	ClassRunning bool
	APOnline     bool
	Capacity     campus.OccupancyCount
}

// FusionResult is the raw (pre-privacy) estimate.
type FusionResult struct {
	People     float64
	Confidence float64
	Spread     float64 // relative half-width of the range, e.g. 0.15 = ±15%
}

// CalculateOccupancy is the core fusion rule. Pure: no state, no clock.
func CalculateOccupancy(in FusionInput, w Weights) FusionResult {
	var r FusionResult
	switch {
	case !in.APOnline && in.ClassRunning:
		// Wi-Fi is blind; the timetable is the best we have.
		r = FusionResult{People: in.Expected, Confidence: 0.35, Spread: 0.35}
	case !in.APOnline:
		// Blind and nothing scheduled. Devices that roamed to neighbouring
		// APs still give a rough figure.
		r = FusionResult{People: in.WiFiPeople, Confidence: 0.25, Spread: 0.5}
	case in.ClassRunning:
		r.People = w.WiFiDuringClass*in.WiFiPeople + (1-w.WiFiDuringClass)*in.Expected
		// Confidence drops when the two sources disagree a lot.
		disagreement := math.Abs(in.WiFiPeople-in.Expected) / math.Max(in.Expected, 10)
		r.Confidence = fn.Clamp(0.9-0.4*disagreement, 0.45, 0.9)
		r.Spread = 0.12
	default:
		r = FusionResult{People: in.WiFiPeople, Confidence: 0.8, Spread: 0.15}
	}
	// Small numbers are proportionally noisier.
	if r.People < 15 {
		r.Confidence = math.Min(r.Confidence, 0.6)
		r.Spread = math.Max(r.Spread, 0.3)
	}
	r.People = math.Max(0, r.People)
	return r
}

// CalculateRoomAvailability returns free seats and the occupancy ratio.
func CalculateRoomAvailability(people float64, capacity campus.OccupancyCount) (free campus.OccupancyCount, ratio float64) {
	if capacity <= 0 {
		return 0, 0
	}
	ratio = people / float64(capacity)
	free = campus.OccupancyCount(math.Max(0, math.Floor(float64(capacity)-people)))
	return free, ratio
}

// Smooth applies an exponentially weighted moving average.
func Smooth(prev, next, alpha float64, hasPrev bool) float64 {
	if !hasPrev {
		return next
	}
	return alpha*next + (1-alpha)*prev
}

// Fusion implements campus.OccupancyEstimator with per-room smoothing.
// It is not safe for concurrent use: exactly one goroutine (the fusion
// stage) owns it, which is cheaper and clearer than a mutex.
type Fusion struct {
	weights      Weights
	offline      func(campus.AccessPointID) bool // injected AP health lookup
	smoothed     map[campus.RoomID]float64
	lastClassful map[campus.RoomID]bool
}

// NewFusion creates an estimator. apOffline reports whether an AP is down.
func NewFusion(w Weights, apOffline func(campus.AccessPointID) bool) *Fusion {
	if apOffline == nil {
		apOffline = func(campus.AccessPointID) bool { return false }
	}
	return &Fusion{weights: w, offline: apOffline, smoothed: map[campus.RoomID]float64{}, lastClassful: map[campus.RoomID]bool{}}
}

// Estimate implements campus.OccupancyEstimator. The returned estimate has
// raw values in Count.Value; the publisher applies privacy afterwards.
func (f *Fusion) Estimate(in campus.EstimateInput) campus.OccupancyEstimate {
	r := in.Room
	expected := ExpectedFromTimetable(in.ActiveEvents, in.At)
	classRunning := expected > 0
	res := CalculateOccupancy(FusionInput{
		WiFiPeople:   EstimateDevicePopulation(in.Devices, in.DevicesPerUser),
		Expected:     expected,
		ClassRunning: classRunning,
		APOnline:     !f.offline(r.PrimaryAP),
		Capacity:     r.Capacity,
	}, f.weights)

	// Reset smoothing when a class starts or ends, so the estimate follows
	// the step change instead of lagging behind it.
	prev, ok := f.smoothed[r.ID]
	if f.lastClassful[r.ID] != classRunning {
		ok = false
	}
	people := Smooth(prev, res.People, f.weights.Smoothing, ok)
	f.smoothed[r.ID] = people
	f.lastClassful[r.ID] = classRunning

	_, ratio := CalculateRoomAvailability(people, r.Capacity)
	n := campus.OccupancyCount(math.Round(people))
	return campus.OccupancyEstimate{
		RoomID:     r.ID,
		Count:      campus.PublicCount{Value: n},
		Low:        campus.OccupancyCount(math.Floor(people * (1 - res.Spread))),
		High:       campus.OccupancyCount(math.Ceil(people * (1 + res.Spread))),
		Capacity:   r.Capacity,
		Ratio:      ratio,
		Level:      campus.LevelForRatio(ratio),
		Confidence: res.Confidence,
		Expected:   campus.OccupancyCount(math.Round(expected)),
		Devices:    campus.PublicCount{Value: campus.OccupancyCount(in.Devices)},
		At:         in.At,
	}
}

// Reset clears the smoothing memory (used when the simulation resets).
func (f *Fusion) Reset() {
	clear(f.smoothed)
	clear(f.lastClassful)
}
