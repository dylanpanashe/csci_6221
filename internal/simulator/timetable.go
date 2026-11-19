package simulator

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// Compile-time check that Schedule satisfies the Timetable interface.
var _ campus.Timetable = (*Schedule)(nil)

// Schedule is one simulated day's timetable. It is immutable once built:
// the simulator swaps in a whole new *Schedule (via an atomic pointer) when
// the day rolls over or a changeover is forced, so readers never see a
// half-updated timetable and need no locks.
type Schedule struct {
	Day    time.Time
	events []campus.TimetableEvent
	byID   map[campus.EventID]int
	byRoom map[campus.RoomID][]int // indexes into events, sorted by start
	// trueAttendance is the share of enrolled students who actually turn up.
	// It is simulator ground truth: the timetable only publishes
	// ExpectedAttendance, which is deliberately a little optimistic.
	trueAttendance map[campus.EventID]float64
}

// slot is a time of day in minutes after midnight.
type slot struct{ start, end int }

func hm(h, m int) int { return h*60 + m }

// Standard GWU-style teaching slots (75 minutes with 20-minute changeovers).
var classSlots = []slot{
	{hm(8, 0), hm(9, 15)}, {hm(9, 35), hm(10, 50)}, {hm(11, 10), hm(12, 25)},
	{hm(12, 45), hm(14, 0)}, {hm(14, 20), hm(15, 35)}, {hm(15, 55), hm(17, 10)},
	{hm(17, 30), hm(18, 45)}, {hm(19, 0), hm(20, 15)},
}

// Lecture halls run their own 80-minute grid.
var lectureSlots = []slot{
	{hm(8, 30), hm(9, 50)}, {hm(10, 10), hm(11, 30)}, {hm(11, 50), hm(13, 10)},
	{hm(13, 30), hm(14, 50)}, {hm(15, 10), hm(16, 30)}, {hm(17, 0), hm(18, 20)},
}

// bookingChance is how likely a room is to be booked in a slot starting at
// the given hour. Late morning and early afternoon are busiest.
func bookingChance(startMin int) float64 {
	switch h := startMin / 60; {
	case h < 9:
		return 0.45
	case h < 15:
		return 0.75
	case h < 17:
		return 0.55
	default:
		return 0.30
	}
}

var courseNames = []string{
	"Intro to Programming", "Data Structures", "Calculus II", "Linear Algebra",
	"Microeconomics", "Organic Chemistry", "Physics I", "Signals & Systems",
	"International Affairs", "Public Policy", "Cybersecurity Foundations",
	"Statistics", "Writing Seminar", "Machine Learning", "Network Defense",
	"Operating Systems", "Ethics", "History of DC",
}

var labNames = []string{"Biology Lab", "Circuits Lab", "Chemistry Lab", "Robotics Lab", "Physics Lab", "Networks Lab"}

var meetingNames = []string{"Faculty Meeting", "Study Group", "Office Hours", "Project Team", "Advising"}

// titleFor picks a course title that suits the room.
func titleFor(t campus.RoomType, n int) string {
	switch t {
	case campus.RoomLab:
		return labNames[n%len(labNames)]
	case campus.RoomMeeting:
		return meetingNames[n%len(meetingNames)]
	default:
		return courseNames[n%len(courseNames)]
	}
}

// demoLecture pins the Bell Hall 101 lecture from the CampusPulse slides:
// about 300 students, 1:30 to 2:50 PM. The demo scenario jumps to 1:40 PM.
var demoLecture = struct {
	room     campus.RoomID
	start    slot
	enrolled campus.OccupancyCount
}{"bell-101", slot{hm(13, 30), hm(14, 50)}, 300}

// quietUntil keeps Bell Hall's third floor free of classes until 4 PM, as in
// the slide-15 example ("3rd floor quiet until 4pm, strong Wi-Fi").
var quietUntil = map[campus.FloorID]int{"bell-3": hm(15, 55)}

// pinned are bookings fixed in every timetable so the demo tells the same
// story whatever the random seed: the floor-3 quiet spell ends at 3:55 PM.
var pinned = map[campus.RoomID]slot{"bell-303": {hm(15, 55), hm(17, 10)}}

// BuildSchedule generates a deterministic timetable for one day.
func BuildSchedule(c *campus.Campus, day time.Time, rng *rand.Rand) *Schedule {
	s := &Schedule{Day: day, byID: map[campus.EventID]int{}, byRoom: map[campus.RoomID][]int{},
		trueAttendance: map[campus.EventID]float64{}}
	at := func(min int) time.Time { return day.Add(time.Duration(min) * time.Minute) }

	n := 0
	for _, r := range c.AllRooms() {
		var slots []slot
		switch r.Type {
		case campus.RoomLectureHall:
			slots = lectureSlots
		case campus.RoomClassroom, campus.RoomLab, campus.RoomMeeting:
			slots = classSlots
		default:
			continue // study rooms, library areas and lobbies are never timetabled
		}
		for _, sl := range slots {
			if q, ok := quietUntil[r.FloorID]; ok && sl.start < q {
				continue
			}
			pinned := (r.ID == demoLecture.room && sl == demoLecture.start) || pinnedSlot(r.ID, sl)
			if !pinned && rng.Float64() > bookingChance(sl.start) {
				continue
			}
			n++
			ev := campus.TimetableEvent{
				ID:         campus.EventID(fmt.Sprintf("%s-%s-%d", day.Format("0102"), r.ID, sl.start)),
				RoomID:     r.ID,
				BuildingID: r.BuildingID,
				Level:      r.Level,
				Title:      titleFor(r.Type, n*7+len(r.ID)),
				Kind:       kindFor(r.Type, rng),
				Window:     campus.Window{Start: at(sl.start), End: at(sl.end)},
			}
			fill := 0.55 + rng.Float64()*0.40
			if r.Type == campus.RoomMeeting {
				fill = 0.4 + rng.Float64()*0.5
			}
			ev.Enrolled = campus.OccupancyCount(float64(r.Capacity) * fill)
			attend := 0.68 + rng.Float64()*0.27 // the real turnout, hidden
			if r.ID == demoLecture.room && sl == demoLecture.start {
				ev.Title, ev.Kind, ev.Enrolled = "Introduction to International Affairs", campus.EventLecture, demoLecture.enrolled
				attend = 0.95
			}
			// The timetable's own expectation is a fixed 85%: like a real
			// registrar it does not know which classes are emptying out.
			ev.ExpectedAttendance = campus.OccupancyCount(float64(ev.Enrolled) * 0.85)
			ev.Cancelled = !pinned && rng.Float64() < 0.04

			s.byID[ev.ID] = len(s.events)
			s.byRoom[r.ID] = append(s.byRoom[r.ID], len(s.events))
			s.trueAttendance[ev.ID] = attend
			s.events = append(s.events, ev)
		}
	}
	s.sortIndexes()
	return s
}

func pinnedSlot(room campus.RoomID, sl slot) bool {
	p, ok := pinned[room]
	return ok && p == sl
}

func kindFor(t campus.RoomType, rng *rand.Rand) campus.EventKind {
	switch t {
	case campus.RoomLectureHall:
		return campus.EventLecture
	case campus.RoomLab:
		return campus.EventLabSession
	case campus.RoomMeeting:
		return campus.EventMeeting
	default:
		if rng.IntN(3) == 0 {
			return campus.EventLecture
		}
		return campus.EventSeminar
	}
}

func (s *Schedule) sortIndexes() {
	for _, idx := range s.byRoom {
		slices.SortFunc(idx, func(a, b int) int { return s.events[a].Window.Start.Compare(s.events[b].Window.Start) })
	}
}

// Event looks up an event by ID.
func (s *Schedule) Event(id campus.EventID) (campus.TimetableEvent, bool) {
	i, ok := s.byID[id]
	if !ok {
		return campus.TimetableEvent{}, false
	}
	return s.events[i], true
}

// Events returns a copy of every event, sorted by start.
func (s *Schedule) Events() []campus.TimetableEvent {
	out := slices.Clone(s.events)
	slices.SortFunc(out, func(a, b campus.TimetableEvent) int { return a.Window.Start.Compare(b.Window.Start) })
	return out
}

// EventsAt implements campus.Timetable.
func (s *Schedule) EventsAt(room campus.RoomID, t time.Time) []campus.TimetableEvent {
	var out []campus.TimetableEvent
	for _, i := range s.byRoom[room] {
		if s.events[i].ActiveAt(t) {
			out = append(out, s.events[i])
		}
	}
	return out
}

// Between implements campus.Timetable.
func (s *Schedule) Between(from, to time.Time) []campus.TimetableEvent {
	w := campus.Window{Start: from, End: to}
	var out []campus.TimetableEvent
	for _, e := range s.events {
		if e.Window.Overlaps(w) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b campus.TimetableEvent) int { return a.Window.Start.Compare(b.Window.Start) })
	return out
}

// RoomEvents returns a room's events in start order.
func (s *Schedule) RoomEvents(room campus.RoomID) []campus.TimetableEvent {
	idx := s.byRoom[room]
	out := make([]campus.TimetableEvent, len(idx))
	for i, j := range idx {
		out[i] = s.events[j]
	}
	return out
}

// Busy reports whether a non-cancelled event in the room overlaps [from, to).
func (s *Schedule) Busy(room campus.RoomID, from, to time.Time) bool {
	w := campus.Window{Start: from, End: to}
	for _, i := range s.byRoom[room] {
		if e := s.events[i]; !e.Cancelled && e.Window.Overlaps(w) {
			return true
		}
	}
	return false
}

// TrueAttendance is simulator ground truth (used by the simulator itself).
func (s *Schedule) TrueAttendance(id campus.EventID) float64 { return s.trueAttendance[id] }

// EndEarly returns a new schedule in which every event running at t ends at
// t. It powers the "simulate class changeover" control. The original
// schedule is untouched.
func (s *Schedule) EndEarly(t time.Time) (*Schedule, int) {
	ns := &Schedule{Day: s.Day, events: slices.Clone(s.events), byID: s.byID, byRoom: s.byRoom, trueAttendance: s.trueAttendance}
	ended := 0
	for i := range ns.events {
		if ns.events[i].ActiveAt(t) {
			ns.events[i].Window.End = t
			ended++
		}
	}
	return ns, ended
}

// Transitions lists events that started or ended in (prev, now].
func (s *Schedule) Transitions(prev, now time.Time) []campus.TimetableTransition {
	var out []campus.TimetableTransition
	for _, e := range s.events {
		if e.Cancelled {
			continue
		}
		if e.Window.Start.After(prev) && !e.Window.Start.After(now) {
			out = append(out, campus.TimetableTransition{Event: e, Started: true})
		}
		if e.Window.End.After(prev) && !e.Window.End.After(now) {
			out = append(out, campus.TimetableTransition{Event: e, Started: false})
		}
	}
	return out
}
