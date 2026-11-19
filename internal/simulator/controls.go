package simulator

import (
	"context"
	"fmt"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/fn"
	"github.com/dylanpanashe/csci_6221/internal/simclock"
)

// commandKind enumerates the simulation controls.
type commandKind int

const (
	cmdPause commandKind = iota
	cmdResume
	cmdReset
	cmdSpeed
	cmdActivity
	cmdChangeover
	cmdFailAP
	cmdRestoreAP
	cmdDemo
)

// command is sent to the Run goroutine, which is the only goroutine allowed
// to change simulation state. The caller waits on reply. This is the
// "share memory by communicating" pattern: no mutex around the simulator.
type command struct {
	kind  commandKind
	value float64
	ap    campus.AccessPointID
	reply chan error
}

// do sends a command to the running loop and waits for the result.
func (s *Simulator) do(ctx context.Context, c command) error {
	if !s.running.Load() {
		return ErrNotRunning
	}
	c.reply = make(chan error, 1) // buffered so the loop never blocks replying
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case s.commands <- c:
	case <-ctx.Done():
		return fmt.Errorf("simulator: command not accepted: %w", ctx.Err())
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return fmt.Errorf("simulator: command timed out: %w", ctx.Err())
	}
}

// apply runs on the Run goroutine.
func (s *Simulator) apply(c command) error {
	switch c.kind {
	case cmdPause:
		s.clock.Pause()
	case cmdResume:
		s.clock.Resume()
	case cmdReset:
		s.clock.Reset()
		s.activity = 1
		s.offline = map[campus.AccessPointID]time.Time{}
		s.faults = nil
		s.pendingAP = map[campus.BuildingID][]apCommand{}
		s.lastTick = s.clock.Now()
		s.startDay(s.lastTick)
		s.reset, s.teleport = true, true
	case cmdSpeed:
		return s.clock.SetSpeed(c.value)
	case cmdActivity:
		s.activity = fn.Clamp(s.activity+c.value, 0.3, 2.0)
	case cmdChangeover:
		ns, ended := s.schedule.Load().EndEarly(s.clock.Now())
		if ended == 0 {
			return &campus.ValidationError{Field: "changeover", Value: s.clock.Now().Format("15:04"), Reason: "no classes are running right now"}
		}
		s.schedule.Store(ns)
	case cmdFailAP, cmdRestoreAP:
		ap, err := s.campus.AccessPoint(c.ap)
		if err != nil {
			return err
		}
		s.pendingAP[ap.BuildingID] = append(s.pendingAP[ap.BuildingID],
			apCommand{ap: ap.ID, fail: c.kind == cmdFailAP, restore: c.kind == cmdRestoreAP})
	case cmdDemo:
		t := simclock.DayAt(s.clock.Now(), s.cfg.Location, 13, 40)
		s.clock.JumpTo(t)
		s.lastTick = t
		if !simclock.DayAt(t, s.cfg.Location, 0, 0).Equal(s.schedule.Load().Day) {
			s.startDay(t)
		}
		s.newDay, s.teleport = true, true // put students straight where they would be at 1:40
	default:
		return fmt.Errorf("simulator: unknown command %d", c.kind)
	}
	return nil
}

// Public controls. Each is a thin wrapper that builds a command.

func (s *Simulator) Pause(ctx context.Context) error  { return s.do(ctx, command{kind: cmdPause}) }
func (s *Simulator) Resume(ctx context.Context) error { return s.do(ctx, command{kind: cmdResume}) }
func (s *Simulator) Reset(ctx context.Context) error  { return s.do(ctx, command{kind: cmdReset}) }

// SetSpeed changes the simulation speed multiplier.
func (s *Simulator) SetSpeed(ctx context.Context, x float64) error {
	return s.do(ctx, command{kind: cmdSpeed, value: x})
}

// AdjustActivity raises or lowers campus activity by delta (clamped 0.3-2.0).
func (s *Simulator) AdjustActivity(ctx context.Context, delta float64) error {
	return s.do(ctx, command{kind: cmdActivity, value: delta})
}

// TriggerChangeover ends every running class now, releasing students into
// lobbies, stairs and lifts.
func (s *Simulator) TriggerChangeover(ctx context.Context) error {
	return s.do(ctx, command{kind: cmdChangeover})
}

// FailAP takes an access point offline at the next tick.
func (s *Simulator) FailAP(ctx context.Context, id campus.AccessPointID) error {
	return s.do(ctx, command{kind: cmdFailAP, ap: id})
}

// RestoreAP brings an access point back at the next tick.
func (s *Simulator) RestoreAP(ctx context.Context, id campus.AccessPointID) error {
	return s.do(ctx, command{kind: cmdRestoreAP, ap: id})
}

// DemoScenario jumps to 1:40 PM, ten minutes into the 300-student Bell Hall
// 101 lecture that ends at 2:50 PM (the example from the proposal).
func (s *Simulator) DemoScenario(ctx context.Context) error {
	return s.do(ctx, command{kind: cmdDemo})
}
