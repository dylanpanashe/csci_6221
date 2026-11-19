package forecast

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
)

var nyc, _ = time.LoadLocation("America/New_York")

func setup(t *testing.T) (*campus.Campus, *simulator.Schedule, time.Time) {
	t.Helper()
	c := campus.NewGWUCampus()
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, nyc)
	return c, simulator.BuildSchedule(c, day, rand.New(rand.NewPCG(1, 2))), day.Add(13*time.Hour + 40*time.Minute)
}

// The slide-15 example: at 1:40 PM, warn that Bell Hall's lobby will be
// packed when the 300-student lecture in Room 101 ends at 2:50 PM.
func TestChangeoverWarningForBell101(t *testing.T) {
	c, sched, now := setup(t)
	people := func(id campus.RoomID) float64 {
		if id == "bell-101" {
			return 284
		}
		return 0
	}
	ws := ChangeoverWarnings(c, sched, now, 3*time.Hour, people, 1, 80)
	var w *ChangeoverWarning
	for i := range ws {
		if ws[i].RoomID == "bell-101" {
			w = &ws[i]
		}
	}
	if w == nil {
		t.Fatal("no warning for bell-101")
	}
	if got := w.EndsAt.Format("15:04"); got != "14:50" {
		t.Errorf("ends at %s", got)
	}
	if w.People != 280 {
		t.Errorf("people = %d, want rounded 280", w.People)
	}
	if w.ArriveBefore.Format("15:04") != "14:45" {
		t.Errorf("arrive before %s, want 14:45", w.ArriveBefore.Format("15:04"))
	}
	if !strings.Contains(strings.Join(w.AffectedSpace, ","), "Lobby") {
		t.Errorf("affected = %v", w.AffectedSpace)
	}
	if w.CrowdedUntil.Sub(w.CrowdedFrom) < 15*time.Minute {
		t.Errorf("a 280-person exit should crowd the lobby for 15+ minutes, got %v", w.CrowdedUntil.Sub(w.CrowdedFrom))
	}
}

func TestQuietUntilBellThirdFloor(t *testing.T) {
	c, sched, now := setup(t)
	b, _ := c.Building("bell")
	q := QuietUntil(sched, b.Floors[2], now)
	if q.IsZero() || q.Hour() < 15 {
		t.Fatalf("Bell floor 3 quiet until %v; expected the first class around 4 PM", q)
	}
}

type fixedProvider struct{ s Snapshot }

func (f fixedProvider) Snapshot() (Snapshot, error) { return f.s, nil }

func TestRuleBasedSeesLectureEnding(t *testing.T) {
	c, sched, now := setup(t)
	snap := Snapshot{At: now, People: map[campus.RoomID]float64{"bell-101": 280}, Timetable: sched}
	e, err := NewEngine(c, fixedProvider{snap}, "rule_based")
	if err != nil {
		t.Fatal(err)
	}
	f, err := e.Forecast(context.Background(), campus.ForecastRequest{Subject: "bell-101", Kind: "room"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Points) != 4 {
		t.Fatalf("points = %d", len(f.Points))
	}
	if f.Points[0].Count < 250 {
		t.Errorf("15 min ahead (1:55 PM) the lecture is still on: got %d", f.Points[0].Count)
	}
	if f.Points[3].Count > 150 {
		t.Errorf("120 min ahead (3:40 PM) the lecture is over: got %d", f.Points[3].Count)
	}
	for _, p := range f.Points {
		if len(p.Factors) == 0 {
			t.Errorf("%d-minute forecast has no explanation", p.Minutes)
		}
	}
	if f.Points[0].Confidence <= f.Points[3].Confidence {
		t.Error("confidence should fall with the horizon")
	}
}

func TestLobbyForecastRisesAtChangeover(t *testing.T) {
	c, sched, now := setup(t)
	snap := Snapshot{At: now, People: map[campus.RoomID]float64{"bell-1-lobby": 20}, Timetable: sched}
	e, _ := NewEngine(c, fixedProvider{snap}, "rule_based")
	r, _ := c.Room("bell-1-lobby")
	pts := e.RoomPoints(snap, r, []time.Duration{15 * time.Minute, 75 * time.Minute}) // 1:55 PM and 2:55 PM
	if pts[1].Count <= pts[0].Count+50 {
		t.Fatalf("lobby at 2:55 PM (%d) should be far busier than at 1:55 PM (%d)", pts[1].Count, pts[0].Count)
	}
}

func TestForecastGroupsAndErrors(t *testing.T) {
	c, sched, now := setup(t)
	e, _ := NewEngine(c, fixedProvider{Snapshot{At: now, People: map[campus.RoomID]float64{}, Timetable: sched}}, "rule_based")
	for _, kind := range []string{"floor", "building", "campus"} {
		subj := map[string]string{"floor": "bell-1", "building": "bell", "campus": "campus"}[kind]
		f, err := e.Forecast(context.Background(), campus.ForecastRequest{Subject: subj, Kind: kind})
		if err != nil || len(f.Points) != 4 {
			t.Errorf("%s forecast: %v", kind, err)
		}
	}
	if _, err := e.Forecast(context.Background(), campus.ForecastRequest{Subject: "nope", Kind: "room"}); !errors.Is(err, campus.ErrNotFound) {
		t.Errorf("unknown room: %v", err)
	}
	if _, err := e.Forecast(context.Background(), campus.ForecastRequest{Kind: "galaxy"}); !errors.Is(err, campus.ErrInvalidInput) {
		t.Errorf("bad kind: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Forecast(ctx, campus.ForecastRequest{Kind: "campus"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: %v", err)
	}
	if _, err := NewEngine(c, nil, "crystal_ball"); err == nil {
		t.Error("unknown strategy accepted")
	}
}

func TestLearnUpdatesAttendanceRatio(t *testing.T) {
	c, sched, now := setup(t)
	e, _ := NewEngine(c, nil, "rule_based")
	ev := sched.EventsAt("bell-101", now)[0]
	for i := 0; i < 40; i++ {
		e.Learn(now.Add(5*time.Minute), map[campus.RoomID]float64{"bell-101": float64(ev.ExpectedAttendance) * 1.1}, sched)
	}
	if r := e.AttendanceRatio(); r < 1.05 || r > 1.11 {
		t.Fatalf("ratio = %v, want about 1.1", r)
	}
}
