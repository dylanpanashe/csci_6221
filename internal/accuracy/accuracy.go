// Package accuracy scores CampusPulse against the simulator's ground truth,
// using the success targets from the project proposal. It is the only part
// of the system, besides the simulator itself, that ever sees true
// headcounts, and it only ever reports aggregate error figures.
package accuracy

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
)

// Metric is one measured result next to its target.
type Metric struct {
	Key     string  `json:"key"`
	Name    string  `json:"name"`
	Target  string  `json:"target"`
	Value   float64 `json:"value"`
	Display string  `json:"display"`
	Samples int     `json:"samples"`
	Met     *bool   `json:"met,omitempty"` // nil until there is enough data
	Note    string  `json:"note,omitempty"`
}

// Report is the full scorecard.
type Report struct {
	At      time.Time `json:"at"`
	Metrics []Metric  `json:"metrics"`
}

// WAPE is the weighted absolute percentage error: total absolute error
// divided by total true value. Unlike a plain average of percentages it is
// not blown up by nearly empty rooms. Pure function.
func WAPE(est, truth map[campus.RoomID]float64) (float64, bool) {
	var errSum, trueSum float64
	for id, t := range truth {
		errSum += math.Abs(est[id] - t)
		trueSum += t
	}
	for id, e := range est {
		if _, ok := truth[id]; !ok {
			errSum += e
		}
	}
	if trueSum < 1 {
		return 0, false
	}
	return errSum / trueSum, true
}

// running is a mean that can be updated one value at a time.
type running struct {
	sum float64
	n   int
}

func (r *running) add(v float64) { r.sum += v; r.n++ }
func (r *running) mean() float64 {
	if r.n == 0 {
		return 0
	}
	return r.sum / float64(r.n)
}

// Evaluator accumulates scores. Its methods are called from different
// engine goroutines, so a mutex guards its state.
type Evaluator struct {
	campus *campus.Campus

	mu         sync.Mutex
	lastAt     time.Time
	occRoom    running
	occBuild   running
	pending    map[string]map[time.Time]map[campus.RoomID]float64 // model -> target -> forecast
	fcRoom     map[string]*running
	fcBuild    map[string]*running
	dzEligible map[campus.AccessPointID]bool
	dzFound    map[campus.AccessPointID]bool
	clsRight   int
	clsTotal   int
	faultsSeen map[faultKey]bool
	delays     []time.Duration
	missed     int
}

type faultKey struct {
	ap    campus.AccessPointID
	start time.Time
}

// New creates an evaluator.
func New(c *campus.Campus) *Evaluator {
	e := &Evaluator{campus: c}
	e.reset()
	return e
}

func (e *Evaluator) reset() {
	e.occRoom, e.occBuild = running{}, running{}
	e.pending = map[string]map[time.Time]map[campus.RoomID]float64{}
	e.fcRoom = map[string]*running{"rule_based": {}, "persistence": {}}
	e.fcBuild = map[string]*running{"rule_based": {}, "persistence": {}}
	e.dzEligible = map[campus.AccessPointID]bool{}
	e.dzFound = map[campus.AccessPointID]bool{}
	e.clsRight, e.clsTotal = 0, 0
	e.faultsSeen = map[faultKey]bool{}
	e.delays = nil
	e.missed = 0
}

// checkJump resets scoring when simulated time goes backwards (reset or
// demo jump). Caller holds the lock.
func (e *Evaluator) checkJump(at time.Time) {
	if !e.lastAt.IsZero() && at.Before(e.lastAt.Add(-time.Minute)) {
		e.reset()
	}
	e.lastAt = at
}

func toFloat(m map[campus.RoomID]campus.OccupancyCount) map[campus.RoomID]float64 {
	out := make(map[campus.RoomID]float64, len(m))
	for k, v := range m {
		out[k] = float64(v)
	}
	return out
}

func (e *Evaluator) byBuilding(m map[campus.RoomID]float64) map[campus.RoomID]float64 {
	out := map[campus.RoomID]float64{}
	for id, v := range m {
		if r, err := e.campus.Room(id); err == nil {
			out[campus.RoomID(r.BuildingID)] += v
		}
	}
	return out
}

// ScoreOccupancy compares raw (pre-noise) estimates with true headcounts.
// Only ticks with real activity are scored, so an empty early-morning
// campus does not dominate the average.
func (e *Evaluator) ScoreOccupancy(est map[campus.RoomID]float64, truth *simulator.Truth) {
	if truth == nil || truth.Seq == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkJump(truth.At)
	tr := toFloat(truth.Rooms)
	if w, ok := WAPE(est, tr); ok && sum(tr) >= 100 {
		e.occRoom.add(w)
	}
	if w, ok := WAPE(e.byBuilding(est), e.byBuilding(tr)); ok && sum(tr) >= 100 {
		e.occBuild.add(w)
	}
	// Score any forecasts whose target time has now arrived.
	for model, byTarget := range e.pending {
		for target, fc := range byTarget {
			if target.After(truth.At) {
				continue
			}
			delete(byTarget, target)
			if truth.At.Sub(target) > 5*time.Minute || sum(tr) < 100 {
				continue // stale (clock jumped) or campus nearly empty
			}
			if w, ok := WAPE(fc, tr); ok {
				e.fcRoom[model].add(w)
			}
			if w, ok := WAPE(e.byBuilding(fc), e.byBuilding(tr)); ok {
				e.fcBuild[model].add(w)
			}
		}
	}
}

// RecordForecast stores a forecast to be scored when its time comes.
func (e *Evaluator) RecordForecast(model string, target time.Time, rooms map[campus.RoomID]float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending[model] == nil {
		e.pending[model] = map[time.Time]map[campus.RoomID]float64{}
	}
	e.pending[model][target] = rooms
}

// ScoreDiagnostics checks AP diagnoses against the simulator's planted dead
// zones, its true AP load and its fault log.
func (e *Evaluator) ScoreDiagnostics(statuses []diagnostics.APStatus, truth *simulator.Truth, minDevices int, timeout time.Duration) {
	if truth == nil || truth.Seq == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := truth.At
	failed := map[campus.AccessPointID]bool{}
	for _, st := range statuses {
		planted := truth.DeadZones[st.AP]
		if planted && st.Devices >= minDevices {
			e.dzEligible[st.AP] = true
		}
		hasDZ := false
		for _, d := range st.All {
			switch d.Kind {
			case campus.DiagnosisDeadZone:
				hasDZ = true
			case campus.DiagnosisAPFailure:
				failed[st.AP] = true
			}
		}
		if planted && hasDZ {
			e.dzFound[st.AP] = true
		}
		switch st.Primary.Kind {
		case campus.DiagnosisDeadZone:
			e.clsTotal++
			if planted {
				e.clsRight++
			}
		case campus.DiagnosisCongestion:
			e.clsTotal++
			if !planted && truth.APLoad[st.AP] >= 0.75 {
				e.clsRight++
			}
		}
	}
	for _, f := range truth.Faults {
		k := faultKey{f.AccessPoint, f.Start}
		if e.faultsSeen[k] || f.Start.After(now) {
			continue
		}
		switch {
		case failed[f.AccessPoint]:
			e.faultsSeen[k] = true
			e.delays = append(e.delays, now.Sub(f.Start))
		case f.End != nil && f.End.Sub(f.Start) > timeout+2*time.Minute:
			e.faultsSeen[k] = true
			e.missed++
		case f.End != nil:
			e.faultsSeen[k] = true // too short to be detectable by design
		}
	}
}

// Report builds the scorecard.
func (e *Evaluator) Report(throughput float64) Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := Report{At: e.lastAt}
	r.Metrics = append(r.Metrics,
		pct("occupancy_error", "Occupancy estimation error (room level)", "< 15%", e.occRoom, 0.15, 20,
			"Weighted absolute % error of estimates vs true headcounts"),
		pct("occupancy_error_building", "Occupancy estimation error (building level)", "< 15%", e.occBuild, 0.15, 20, ""),
		pct("forecast_60_error", "60-minute forecast error (building level)", "< 20%", *e.fcBuild["rule_based"], 0.20, 10,
			"Rule-based model"),
		pct("forecast_60_error_room", "60-minute forecast error (room level)", "< 20%", *e.fcRoom["rule_based"], 0.20, 10, ""),
		pct("forecast_60_baseline", "60-minute forecast error, naive baseline", "for comparison", *e.fcBuild["persistence"], -1, 10,
			"Assumes nothing changes; shows what the model adds"),
	)

	dz := Metric{Key: "dead_zone_detection", Name: "Planted dead zones found", Target: "≥ 90%", Samples: len(e.dzEligible)}
	if n := len(e.dzEligible); n > 0 {
		found := 0
		for ap := range e.dzEligible {
			if e.dzFound[ap] {
				found++
			}
		}
		dz.Value = float64(found) / float64(n)
		dz.Display = fmtPct(dz.Value)
		dz.Met = boolPtr(dz.Value >= 0.9)
		dz.Note = "Only dead zones that had devices in them so far can be found"
	}
	r.Metrics = append(r.Metrics, dz)

	cls := Metric{Key: "classification", Name: "Congestion vs dead zone classification", Target: "≥ 85%", Samples: e.clsTotal}
	if e.clsTotal >= 10 {
		cls.Value = float64(e.clsRight) / float64(e.clsTotal)
		cls.Display = fmtPct(cls.Value)
		cls.Met = boolPtr(cls.Value >= 0.85)
	}
	r.Metrics = append(r.Metrics, cls)

	fail := Metric{Key: "ap_failure_detection", Name: "Failed AP detection time", Target: "< 5 min", Samples: len(e.delays)}
	if len(e.delays) > 0 {
		var worst time.Duration
		var total time.Duration
		for _, d := range e.delays {
			total += d
			worst = max(worst, d)
		}
		mean := total / time.Duration(len(e.delays))
		fail.Value = mean.Minutes()
		fail.Display = mean.Round(time.Second).String() + " mean, " + worst.Round(time.Second).String() + " worst"
		fail.Met = boolPtr(worst < 5*time.Minute && e.missed == 0)
		if e.missed > 0 {
			fail.Note = itoa(e.missed) + " outage(s) missed"
		}
	}
	r.Metrics = append(r.Metrics, fail)

	tp := Metric{Key: "throughput", Name: "Wi-Fi readings processed per second (live)", Target: "≈ 50,000 capacity", Value: throughput,
		Display: itoa(int(throughput)) + "/s", Note: "Live rate is limited by the simulator, not the pipeline; see the benchmarks for capacity"}
	r.Metrics = append(r.Metrics, tp)
	return r
}

func pct(key, name, target string, r running, limit float64, minN int, note string) Metric {
	m := Metric{Key: key, Name: name, Target: target, Samples: r.n, Note: note}
	if r.n >= minN {
		m.Value = r.mean()
		m.Display = fmtPct(m.Value)
		if limit > 0 {
			m.Met = boolPtr(m.Value < limit)
		}
	}
	return m
}

func sum(m map[campus.RoomID]float64) float64 {
	var s float64
	for _, v := range m {
		s += v
	}
	return s
}

func boolPtr(b bool) *bool { return &b }

func fmtPct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func itoa(n int) string { return strconv.Itoa(n) }
