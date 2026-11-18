// Package simclock provides the accelerated, pausable clock that drives the
// simulation, plus a fixed clock for tests. Both satisfy campus.Clock.
package simclock

import (
	"fmt"
	"sync"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// Compile-time interface assertions: the build fails if either type stops
// satisfying campus.Clock.
var (
	_ campus.Clock = (*SimClock)(nil)
	_ campus.Clock = Fixed{}
)

// SimClock maps real time onto simulated campus time.
//
//	sim = anchorSim + (real - anchorReal) * speed
//
// Changing speed or pausing re-anchors the clock so simulated time never
// jumps. Reads (Now) vastly outnumber writes, hence the RWMutex.
type SimClock struct {
	mu         sync.RWMutex
	real       func() time.Time // injectable real-time source
	anchorReal time.Time
	anchorSim  time.Time
	speed      float64
	paused     bool
	start      time.Time // for Reset
}

// New creates a clock starting at simStart and running speed times faster
// than real time. realNow is usually time.Now; tests pass their own function.
func New(simStart time.Time, speed float64, realNow func() time.Time) (*SimClock, error) {
	if speed <= 0 {
		return nil, &campus.ValidationError{Field: "speed", Value: speed, Reason: "must be positive"}
	}
	if realNow == nil {
		realNow = time.Now
	}
	return &SimClock{real: realNow, anchorReal: realNow(), anchorSim: simStart, speed: speed, start: simStart}, nil
}

// Now returns the current simulated time.
func (c *SimClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nowLocked()
}

func (c *SimClock) nowLocked() time.Time {
	if c.paused {
		return c.anchorSim
	}
	elapsed := c.real().Sub(c.anchorReal)
	return c.anchorSim.Add(time.Duration(float64(elapsed) * c.speed))
}

// reanchor must be called with the write lock held.
func (c *SimClock) reanchor() {
	c.anchorSim = c.nowLocked()
	c.anchorReal = c.real()
}

// Pause freezes simulated time.
func (c *SimClock) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		c.reanchor()
		c.paused = true
	}
}

// Resume restarts simulated time from where it was paused.
func (c *SimClock) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		c.anchorReal = c.real()
		c.paused = false
	}
}

// Paused reports whether the clock is paused.
func (c *SimClock) Paused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

// SetSpeed changes the acceleration factor without a jump in simulated time.
func (c *SimClock) SetSpeed(speed float64) error {
	if speed <= 0 || speed > 3600 {
		return &campus.ValidationError{Field: "speed", Value: speed, Reason: "must be between 0 and 3600"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reanchor()
	c.speed = speed
	return nil
}

// Speed returns the current acceleration factor.
func (c *SimClock) Speed() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.speed
}

// JumpTo moves simulated time to t (used by the demo scenario controls).
func (c *SimClock) JumpTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.anchorSim = t
	c.anchorReal = c.real()
}

// Reset returns the clock to its original start time and leaves it running.
func (c *SimClock) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.anchorSim = c.start
	c.anchorReal = c.real()
	c.paused = false
}

func (c *SimClock) String() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	state := "running"
	if c.paused {
		state = "paused"
	}
	return fmt.Sprintf("SimClock(%s, x%.0f, %s)", c.nowLocked().Format("15:04:05"), c.speed, state)
}

// Fixed is a clock that always returns the same instant. Value receiver:
// it is small and immutable, so copying it is free and safe.
type Fixed struct{ T time.Time }

func (f Fixed) Now() time.Time { return f.T }

// DayAt returns today's date in loc at the given hour and minute. Used to
// start the simulation at a sensible hour of the teaching day.
func DayAt(now time.Time, loc *time.Location, hour, min int) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, hour, min, 0, 0, loc)
}
