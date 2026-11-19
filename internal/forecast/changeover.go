package forecast

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// ChangeoverWarning predicts a crowd in a building's shared spaces when a
// large class lets out.
type ChangeoverWarning struct {
	BuildingID    campus.BuildingID `json:"building_id"`
	Building      string            `json:"building"`
	RoomID        campus.RoomID     `json:"room_id"`
	Room          string            `json:"room"`
	Event         string            `json:"event"`
	EndsAt        time.Time         `json:"ends_at"`
	People        int               `json:"people"` // rounded to the nearest 10
	CrowdedFrom   time.Time         `json:"crowded_from"`
	CrowdedUntil  time.Time         `json:"crowded_until"`
	AffectedSpace []string          `json:"affected_spaces"`
	ArriveBefore  time.Time         `json:"arrive_before"`
	ArriveAfter   time.Time         `json:"arrive_after"`
	Advice        []string          `json:"advice"`
}

// ChangeoverWarnings lists predicted changeover crowds in the next
// lookahead window. people returns the current raw estimate for a running
// class; for classes that have not started, the expected attendance is used.
// Head counts are rounded to the nearest 10 before they leave this
// function, and classes under minPeople are ignored.
func ChangeoverWarnings(c *campus.Campus, tt campus.Timetable, now time.Time, lookahead time.Duration,
	people func(campus.RoomID) float64, ratio float64, minPeople int) []ChangeoverWarning {

	var out []ChangeoverWarning
	for _, e := range tt.Between(now, now.Add(lookahead)) {
		if e.Cancelled || e.Window.End.Before(now) || e.Window.End.After(now.Add(lookahead)) {
			continue
		}
		n := float64(e.ExpectedAttendance) * ratio
		if e.ActiveAt(now) && people != nil {
			n = people(e.RoomID)
		}
		if int(n) < minPeople {
			continue
		}
		r, err := c.Room(e.RoomID)
		if err != nil {
			continue
		}
		b, _ := c.Building(r.BuildingID)
		// Crowd lasts longer for bigger classes (lift and stair queues).
		clear := time.Duration(8+math.Min(n/25, 12)) * time.Minute
		w := ChangeoverWarning{
			BuildingID: b.ID, Building: b.Name, RoomID: r.ID, Room: r.Name, Event: e.Title,
			EndsAt:       e.Window.End,
			People:       int(math.Round(n/10) * 10),
			CrowdedFrom:  e.Window.End,
			CrowdedUntil: e.Window.End.Add(clear),
			ArriveBefore: e.Window.End.Add(-5 * time.Minute),
			ArriveAfter:  e.Window.End.Add(clear + 5*time.Minute),
		}
		spaces := []string{b.Floors[0].Lobby.Name}
		if r.Level != 1 {
			spaces = append([]string{b.Floors[r.Level-1].Lobby.Name}, spaces...)
		}
		w.AffectedSpace = spaces
		w.Advice = []string{
			fmt.Sprintf("Arrive before %s or after %s", clock(w.ArriveBefore), clock(w.ArriveAfter)),
			fmt.Sprintf("Avoid the lobby, stairs and lifts between %s and %s", clock(w.CrowdedFrom), clock(w.CrowdedUntil)),
		}
		out = append(out, w)
	}
	slices.SortFunc(out, func(a, b ChangeoverWarning) int { return a.EndsAt.Compare(b.EndsAt) })
	return out
}

// QuietUntil returns when the next class starts on a floor after now, or
// the zero time if none is scheduled for the rest of the day. "Bell Hall
// floor 3 is quiet until 4 PM" comes from this.
func QuietUntil(tt campus.Timetable, f *campus.Floor, now time.Time) time.Time {
	end := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 0, 0, now.Location())
	var next time.Time
	for _, e := range tt.Between(now, end) {
		if e.Cancelled || e.BuildingID != f.BuildingID || e.Level != f.Level {
			continue
		}
		if e.ActiveAt(now) {
			return now
		}
		if next.IsZero() || e.Window.Start.Before(next) {
			next = e.Window.Start
		}
	}
	return next
}
