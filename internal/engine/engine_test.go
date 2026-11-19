package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/pipeline"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
	"github.com/dylanpanashe/csci_6221/internal/simclock"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
	"github.com/dylanpanashe/csci_6221/internal/store"
)

// End-to-end: simulator → pipeline → engine → store, at high speed.
func TestEngineEndToEnd(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	loc, _ := time.LoadLocation("America/New_York")
	c := campus.NewGWUCampus()
	clk, _ := simclock.New(time.Date(2026, 11, 18, 13, 0, 0, 0, loc), 3600, nil)
	sim, err := simulator.New(simulator.Config{Population: 2600, Seed: 3, Tick: 33 * time.Millisecond, Location: loc, FailRate: -1}, c, clk, quiet)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := privacy.NewHasher("t", loc)
	p, _ := pipeline.New(pipeline.Config{Workers: 4}, c, h, quiet)
	st := store.New(100)
	eng, err := New(DefaultConfig(), c, sim, st, quiet)
	if err != nil {
		t.Fatal(err)
	}
	updates, unsub := eng.Updates.Subscribe(4096)
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { eng.Run(ctx, p.Start(ctx, sim)); close(done) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("estimates", func() bool { _, err := st.Group("campus"); return err == nil && len(st.Statuses()) > 0 })

	// Fail an AP and time the alert in simulated minutes.
	const victim campus.AccessPointID = "gelman-2-w"
	if err := sim.FailAP(context.Background(), victim); err != nil {
		t.Fatal(err)
	}
	failedAt := clk.Now()
	var alert campus.Alert
	waitFor("failure alert", func() bool {
		for _, a := range st.Alerts(true) {
			if a.AccessPoint == victim && a.Kind == campus.DiagnosisAPFailure {
				alert = a
				return true
			}
		}
		return false
	})
	if d := alert.RaisedAt.Sub(failedAt); d > 5*time.Minute {
		t.Errorf("failure detected %v after it happened; target < 5 min", d)
	}
	if len(alert.AffectedRooms) == 0 {
		t.Error("alert lists no affected rooms")
	}

	// Privacy: nothing published may show a count under 10.
	for _, e := range st.RoomEstimates() {
		if !e.Count.Suppressed && e.Count.Value < 10 {
			t.Fatalf("room %s published %d", e.RoomID, e.Count.Value)
		}
		if e.Count.Suppressed && (!e.Devices.Suppressed || e.Devices.Value != 0 || e.Ratio != 0 || e.Low != 0 || e.High != 0) {
			t.Fatalf("room %s hides its count but leaks it another way: %+v", e.RoomID, e)
		}
	}
	waitFor("forecasts", func() bool { _, ok := st.Forecast("building", "bell"); return ok })
	waitFor("history", func() bool { return len(st.Series("campus.occupancy")["campus.occupancy"]) > 3 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop")
	}
	kinds := map[string]bool{}
	for msg := range updates { // closed by the engine on exit
		kinds[msg.Type] = true
	}
	for _, k := range []string{"occupancy", "wifi", "alert", "frame", "forecast"} {
		if !kinds[k] {
			t.Errorf("no %q message was published", k)
		}
	}
}

func TestCoarsenFlows(t *testing.T) {
	out := CoarsenFlows([]campus.MovementFlow{{Count: 2}, {Count: 3}, {Count: 12}, {Count: 48}})
	got := []campus.OccupancyCount{}
	for _, f := range out {
		got = append(got, f.Count)
	}
	if len(got) != 3 || got[0] != 5 || got[1] != 10 || got[2] != 50 {
		t.Fatalf("got %v", got)
	}
}

func TestOfferKeepsLatest(t *testing.T) {
	ch := make(chan int, 1)
	offer(ch, 1)
	offer(ch, 2)
	if v := <-ch; v != 2 {
		t.Fatalf("got %d, want latest value 2", v)
	}
}
