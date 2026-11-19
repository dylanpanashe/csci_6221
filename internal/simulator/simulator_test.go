package simulator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/simclock"
)

var nyc, _ = time.LoadLocation("America/New_York")

func testDay() time.Time { return time.Date(2026, 11, 18, 0, 0, 0, 0, nyc) }

func TestScheduleHasDemoLectureAndQuietFloor(t *testing.T) {
	c := campus.NewGWUCampus()
	s := BuildSchedule(c, testDay(), rand.New(rand.NewPCG(1, 2)))
	at140 := testDay().Add(13*time.Hour + 40*time.Minute)
	evs := s.EventsAt("bell-101", at140)
	if len(evs) != 1 {
		t.Fatalf("bell-101 at 1:40 PM has %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Enrolled != 300 || ev.Window.End.Format("15:04") != "14:50" {
		t.Errorf("demo lecture = %d enrolled, ends %s", ev.Enrolled, ev.Window.End.Format("15:04"))
	}
	b, _ := c.Building("bell")
	if q := firstClassOnFloor(s, b.Floors[2]); q != "15:55" {
		t.Errorf("Bell floor 3's first class starts at %s, want 15:55 for every seed", q)
	}
	for _, id := range []campus.RoomID{"bell-301", "bell-302", "bell-303", "bell-304"} {
		for _, e := range s.RoomEvents(id) {
			if e.Window.Start.Hour() < 15 {
				t.Errorf("%s has a class at %s; Bell floor 3 should be quiet until 4 PM", id, e.Window.Start.Format("15:04"))
			}
		}
	}
}

func TestScheduleIsDeterministic(t *testing.T) {
	c := campus.NewGWUCampus()
	a := BuildSchedule(c, testDay(), rand.New(rand.NewPCG(7, 7))).Events()
	b := BuildSchedule(c, testDay(), rand.New(rand.NewPCG(7, 7))).Events()
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Enrolled != b[i].Enrolled {
			t.Fatalf("event %d differs", i)
		}
	}
}

func TestEndEarlyLeavesOriginalUntouched(t *testing.T) {
	c := campus.NewGWUCampus()
	s := BuildSchedule(c, testDay(), rand.New(rand.NewPCG(1, 2)))
	at := testDay().Add(14 * time.Hour)
	ns, ended := s.EndEarly(at)
	if ended == 0 {
		t.Fatal("expected running classes at 2 PM")
	}
	if len(ns.EventsAt("bell-101", at)) != 0 {
		t.Error("new schedule should have no running lecture")
	}
	if len(s.EventsAt("bell-101", at)) != 1 {
		t.Error("original schedule was modified")
	}
}

// harness runs a simulator with drained feeds and returns the frames channel.
type harness struct {
	sim    *Simulator
	clock  *simclock.SimClock
	frames chan campus.SimFrame
	tel    chan campus.TelemetryBatch
	cancel context.CancelFunc
	done   chan error
}

func startSim(t *testing.T, demo bool, speed float64) *harness {
	t.Helper()
	clk, err := simclock.New(testDay().Add(8*time.Hour), speed, nil)
	if err != nil {
		t.Fatal(err)
	}
	sim, err := New(Config{Population: 2600, Seed: 42, Shards: 4, Tick: 15 * time.Millisecond, Location: nyc, Demo: demo, FailRate: -1},
		campus.NewGWUCampus(), clk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{sim: sim, clock: clk, frames: make(chan campus.SimFrame, 1024), tel: make(chan campus.TelemetryBatch, 1024), cancel: cancel, done: make(chan error, 1)}
	obs := make(chan campus.ObservationBatch, 64)
	go func() {
		for range obs { // drain observations
		}
	}()
	go func() {
		h.done <- sim.Run(ctx, campus.Feeds{Observations: obs, Telemetry: h.tel, Frames: h.frames})
		close(obs)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

// waitUntil reads frames until the simulated clock passes t.
func (h *harness) waitUntil(t *testing.T, until time.Time, each func(*Truth)) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f := <-h.frames:
			if each != nil {
				each(h.sim.Truth())
			}
			if !f.At.Before(until) {
				return
			}
		case <-deadline:
			t.Fatalf("simulation did not reach %s", until.Format("15:04"))
		}
	}
}

func TestDemoScenarioChangeoverCrowd(t *testing.T) {
	// 15 ms ticks at x2400 is one simulated minute per real 25 ms.
	h := startSim(t, true, 2400)
	day := testDay()
	h.waitUntil(t, day.Add(13*time.Hour+45*time.Minute), nil)

	truth := h.sim.Truth()
	if n := truth.Rooms["bell-101"]; n < 230 || n > 300 {
		t.Errorf("bell-101 true headcount at ~1:45 PM = %d, want roughly 250-290", n)
	}
	before := truth.Rooms["bell-1-lobby"]

	var peak campus.OccupancyCount
	h.waitUntil(t, day.Add(15*time.Hour+5*time.Minute), func(tr *Truth) {
		if tr.At.After(day.Add(14*time.Hour+49*time.Minute)) && tr.Rooms["bell-1-lobby"] > peak {
			peak = tr.Rooms["bell-1-lobby"]
		}
	})
	if peak < 3*max(before, 20) {
		t.Errorf("Bell lobby peak after 2:50 PM = %d (before: %d); expected a changeover crowd", peak, before)
	}
	t.Logf("bell-101=%d, lobby before=%d, lobby peak 2:50-3:05=%d", truth.Rooms["bell-101"], before, peak)
}

func TestFailAndRestoreAP(t *testing.T) {
	h := startSim(t, false, 600)
	ctx := context.Background()
	const ap campus.AccessPointID = "bell-1-l"
	waitForAP := func(present bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case b := <-h.tel:
				found := false
				for _, s := range b.Samples {
					found = found || s.AccessPoint == ap
				}
				if found == present {
					return
				}
			case <-deadline:
				t.Fatalf("AP %s present=%v never observed", ap, present)
			}
		}
	}
	waitForAP(true)
	if err := h.sim.FailAP(ctx, ap); err != nil {
		t.Fatal(err)
	}
	waitForAP(false)
	if _, down := h.sim.Truth().OfflineAPs[ap]; !down {
		t.Error("ground truth should record the failure")
	}
	if err := h.sim.RestoreAP(ctx, ap); err != nil {
		t.Fatal(err)
	}
	waitForAP(true)
	if err := h.sim.FailAP(ctx, "no-such-ap"); !errors.Is(err, campus.ErrNotFound) {
		t.Errorf("unknown AP: %v", err)
	}
}

func TestControlsNeedRunningSimulator(t *testing.T) {
	clk, _ := simclock.New(testDay().Add(9*time.Hour), 60, nil)
	sim, _ := New(Config{Population: 10, Location: nyc}, campus.NewGWUCampus(), clk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := sim.Pause(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestPauseSpeedAndChangeover(t *testing.T) {
	h := startSim(t, true, 600)
	ctx := context.Background()
	h.waitUntil(t, testDay().Add(13*time.Hour+41*time.Minute), nil)
	if err := h.sim.TriggerChangeover(ctx); err != nil {
		t.Fatalf("changeover during classes: %v", err)
	}
	if len(h.sim.Schedule().EventsAt("bell-101", h.clock.Now())) != 0 {
		t.Error("lecture should have ended")
	}
	if err := h.sim.SetSpeed(ctx, -5); !errors.Is(err, campus.ErrInvalidInput) {
		t.Errorf("negative speed: %v", err)
	}
	if err := h.sim.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	frozen := h.clock.Now()
	time.Sleep(50 * time.Millisecond)
	if !h.clock.Now().Equal(frozen) {
		t.Error("clock moved while paused")
	}
	if err := h.sim.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.sim.AdjustActivity(ctx, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := h.sim.Reset(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsCleanly(t *testing.T) {
	before := runtime.NumGoroutine()
	h := startSim(t, false, 60)
	h.waitUntil(t, testDay().Add(8*time.Hour), nil)
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
		h.done <- nil // let the cleanup's receive succeed
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
	time.Sleep(30 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines before=%d after=%d", before, after)
	}
}

// Students choosing study spaces must never pack a room beyond its seats,
// even though four shards choose independently in the same tick.
func TestNoStudySpaceOverCapacity(t *testing.T) {
	h := startSim(t, true, 2400)
	day := testDay()
	worst := ""
	h.waitUntil(t, day.Add(16*time.Hour), func(tr *Truth) {
		for _, r := range h.sim.campus.AllRooms() {
			if r.IsCirculation() || r.Type == campus.RoomLectureHall {
				continue
			}
			if tr.Rooms[r.ID] > r.Capacity && worst == "" {
				worst = fmt.Sprintf("%s at %s: %d people in %d seats", r.ID, tr.At.Format("15:04"), tr.Rooms[r.ID], r.Capacity)
			}
		}
	})
	if worst != "" {
		t.Fatal(worst)
	}
}

func firstClassOnFloor(s *Schedule, f *campus.Floor) string {
	first := ""
	for _, r := range f.Rooms {
		for _, e := range s.RoomEvents(r.ID) {
			if t := e.Window.Start.Format("15:04"); !e.Cancelled && (first == "" || t < first) {
				first = t
			}
		}
	}
	return first
}
