package simulator

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// offCampus is the location of a student who is not in any building.
const offCampus campus.RoomID = ""

// StudentPlan is one student's day. Plans are built by the simulator loop
// at the start of each day and are read-only afterwards, so the shard that
// owns the student can read its plan without locks.
type StudentPlan struct {
	Arrive, Leave time.Time
	Classes       []ClassSlot // sorted by start
	DropIn        bool        // no classes; comes in only to study
}

// ClassSlot links a student to a timetable event.
type ClassSlot struct {
	Event   campus.EventID
	Room    campus.RoomID
	Attends bool
	Early   time.Duration // arrives this long before the start
	Linger  time.Duration // leaves this long after the end
}

// leg is one step of a journey: be in Room (a lobby, a room, or outdoors)
// until Until.
type leg struct {
	room     campus.RoomID
	outdoors bool
	until    time.Time
}

// student is mutable simulation state. Each student belongs to exactly one
// shard, and only that shard's goroutine ever touches it.
type student struct {
	idx        int
	devices    []string // raw device IDs; immutable after creation, safe to share
	plan       *StudentPlan
	loc        campus.RoomID
	outdoors   bool
	legs       []leg // remaining journey; empty when settled
	dest       campus.RoomID
	spot       campus.RoomID // chosen study spot during a free period
	spotUntil  time.Time
	dropInRoll float64 // fixed per student; compared with the activity level
}

// newStudents creates the population with a realistic spread of devices:
// some students have none, most have a phone and a laptop.
func newStudents(n int, rng *rand.Rand) []*student {
	out := make([]*student, n)
	for i := range out {
		d := 0
		switch r := rng.Float64(); {
		case r < 0.06:
			d = 0
		case r < 0.31:
			d = 1
		case r < 0.81:
			d = 2
		default:
			d = 3
		}
		devs := make([]string, d)
		for k := range devs {
			// Looks like a locally administered MAC. Never leaves the
			// simulator and the first pipeline stage.
			devs[k] = fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", k, (i>>16)&0xff, (i>>8)&0xff, i&0xff, rng.IntN(256))
		}
		out[i] = &student{idx: i, devices: devs, dropInRoll: rng.Float64()}
	}
	return out
}

// buildPlans enrols students in the day's events and sets arrival and
// departure times. It runs on the simulator loop goroutine only.
func buildPlans(s *Schedule, students int, rng *rand.Rand) []*StudentPlan {
	plans := make([]*StudentPlan, students)
	busy := make([][]campus.Window, students)

	events := s.Events()
	for _, ev := range events {
		if ev.Enrolled == 0 {
			continue
		}
		attendRate := s.TrueAttendance(ev.ID)
		need := int(ev.Enrolled)
		for tries := 0; need > 0 && tries < int(ev.Enrolled)*6; tries++ {
			i := rng.IntN(students)
			if overlaps(busy[i], ev.Window) {
				continue
			}
			busy[i] = append(busy[i], ev.Window)
			if plans[i] == nil {
				plans[i] = &StudentPlan{}
			}
			plans[i].Classes = append(plans[i].Classes, ClassSlot{
				Event:   ev.ID,
				Room:    ev.RoomID,
				Attends: !ev.Cancelled && rng.Float64() < attendRate,
				Early:   time.Duration(rng.IntN(10)) * time.Minute,
				Linger:  time.Duration(rng.IntN(4)) * time.Minute,
			})
			need--
		}
	}

	dayStart := s.Day
	for i := range plans {
		p := plans[i]
		if p == nil {
			// No classes today: a drop-in student who may come to study.
			start := dayStart.Add(time.Duration(hm(9, 0)+rng.IntN(5*60)) * time.Minute)
			plans[i] = &StudentPlan{DropIn: true, Arrive: start, Leave: start.Add(time.Duration(60+rng.IntN(240)) * time.Minute)}
			continue
		}
		slices.SortFunc(p.Classes, func(a, b ClassSlot) int {
			ea, _ := s.Event(a.Event)
			eb, _ := s.Event(b.Event)
			return ea.Window.Start.Compare(eb.Window.Start)
		})
		first, _ := s.Event(p.Classes[0].Event)
		last, _ := s.Event(p.Classes[len(p.Classes)-1].Event)
		p.Arrive = first.Window.Start.Add(-time.Duration(15+rng.IntN(60)) * time.Minute)
		p.Leave = last.Window.End.Add(time.Duration(rng.IntN(150)) * time.Minute)
	}
	return plans
}

func overlaps(ws []campus.Window, w campus.Window) bool {
	// Leave 10 minutes between classes for walking.
	padded := campus.Window{Start: w.Start.Add(-10 * time.Minute), End: w.End.Add(10 * time.Minute)}
	for _, x := range ws {
		if x.Overlaps(padded) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Shard worker
// ---------------------------------------------------------------------------

// shardInput is everything a shard needs for one tick. Every field is either
// a value or points at immutable data.
type shardInput struct {
	seq      uint64
	at       time.Time
	schedule *Schedule
	plans    []*StudentPlan // indexed by global student index; nil unless the day changed
	prev     *Truth         // last tick's ground truth (read-only)
	activity float64
	teleport bool
	shards   int // how many shards share the seat budget
}

// presence says a student is in a room with these devices.
type presence struct {
	room    campus.RoomID
	devices []string
}

type flowKey struct {
	from, to campus.RoomID
	outdoors bool
}

// shardOutput is what a shard reports back for one tick.
type shardOutput struct {
	seq      uint64
	counts   map[campus.RoomID]campus.OccupancyCount
	outdoors int
	present  []presence
	flows    map[flowKey]campus.OccupancyCount
	heading  map[campus.RoomID]campus.OccupancyCount // students whose destination is the room
}

// shard owns a slice of the student population. Only its run goroutine
// touches its students, so no locking is needed.
type shard struct {
	id       int
	students []*student
	campus   *campus.Campus
	spots    []studySpot // immutable candidate list
	rng      *rand.Rand
	claimed  map[campus.RoomID]int // seats this shard has handed out this tick
}

type studySpot struct {
	room   *campus.Room
	weight float64
}

func newShard(id int, c *campus.Campus, students []*student, seed uint64) *shard {
	sh := &shard{id: id, students: students, campus: c, rng: rand.New(rand.NewPCG(seed, uint64(id)+1))}
	for _, r := range c.AllRooms() {
		w := 0.0
		switch r.Type {
		case campus.RoomLibraryArea:
			w = 3
		case campus.RoomStudyRoom:
			w = 2.5
		case campus.RoomClassroom:
			w = 0.8
		case campus.RoomMeeting:
			w = 0.4
		}
		if w > 0 && sh.deadZone(r) {
			w *= 0.2 // regulars know the Wi-Fi there is bad and rarely consider it
		}
		if w > 0 {
			sh.spots = append(sh.spots, studySpot{room: r, weight: w})
		}
	}
	return sh
}

// run processes ticks until in is closed. It is a classic worker loop:
// receive from a receive-only channel, send to a send-only channel.
func (sh *shard) run(in <-chan shardInput, out chan<- shardOutput) {
	for inp := range in {
		out <- sh.step(inp)
	}
}

func (sh *shard) step(in shardInput) shardOutput {
	out := shardOutput{
		seq:     in.seq,
		counts:  make(map[campus.RoomID]campus.OccupancyCount, 64),
		flows:   make(map[flowKey]campus.OccupancyCount),
		heading: make(map[campus.RoomID]campus.OccupancyCount, 64),
	}
	sh.claimed = make(map[campus.RoomID]int, 16)
	for _, st := range sh.students {
		if in.plans != nil {
			st.plan = in.plans[st.idx]
			st.spot, st.legs, st.dest = "", nil, st.loc
		}
		from, fromOut := st.loc, st.outdoors
		if in.teleport {
			st.dest = sh.target(st, in)
			st.loc, st.outdoors, st.legs = st.dest, false, nil
			from, fromOut = st.loc, st.outdoors // no flow for a teleport
		} else {
			sh.advance(st, in)
		}
		if st.loc != from || st.outdoors != fromOut {
			out.flows[flowKey{from: from, to: st.loc, outdoors: st.outdoors}]++
		}
		if st.dest != offCampus {
			out.heading[st.dest]++
		}
		switch {
		case st.outdoors:
			out.outdoors++
		case st.loc != offCampus:
			out.counts[st.loc]++
			if len(st.devices) > 0 {
				out.present = append(out.present, presence{room: st.loc, devices: st.devices})
			}
		}
	}
	return out
}

// advance moves one student along their journey and re-plans if their
// target has changed.
func (sh *shard) advance(st *student, in shardInput) {
	// Walk through any legs that have finished.
	for len(st.legs) > 0 && !in.at.Before(st.legs[0].until) {
		st.legs = st.legs[1:]
		if len(st.legs) > 0 {
			st.loc, st.outdoors = st.legs[0].room, st.legs[0].outdoors
		} else {
			st.loc, st.outdoors = st.dest, false
		}
	}

	target := sh.target(st, in)
	if target != st.dest {
		st.dest = target
		st.legs = sh.route(st.loc, st.outdoors, target, in)
		if len(st.legs) > 0 {
			st.loc, st.outdoors = st.legs[0].room, st.legs[0].outdoors
		} else {
			st.loc, st.outdoors = target, false
		}
	}
}

// target decides where a student wants to be at time t.
func (sh *shard) target(st *student, in shardInput) campus.RoomID {
	p, t := st.plan, in.at
	if p == nil || t.Before(p.Arrive) || !t.Before(p.Leave) {
		return offCampus
	}
	if p.DropIn && st.dropInRoll > 0.55*in.activity {
		return offCampus // low activity keeps drop-in students away
	}
	var next *campus.TimetableEvent
	for _, c := range p.Classes {
		ev, ok := in.schedule.Event(c.Event)
		if !ok || ev.Cancelled || !c.Attends {
			continue
		}
		if !t.Before(ev.Window.Start.Add(-c.Early)) && t.Before(ev.Window.End.Add(c.Linger)) {
			return c.Room
		}
		if ev.Window.Start.After(t) && next == nil {
			e := ev
			next = &e
		}
	}

	// Free period. Wait in the lobby if the next class is close.
	if next != nil && next.Window.Start.Sub(t) <= 20*time.Minute {
		if r, err := sh.campus.Room(next.RoomID); err == nil {
			if f, err := sh.campus.FloorByID(r.FloorID); err == nil {
				return f.Lobby.ID
			}
		}
	}
	if st.spot != "" && t.Before(st.spotUntil) && !in.schedule.Busy(st.spot, t, t.Add(15*time.Minute)) {
		return st.spot
	}
	// Long gaps: some students leave campus for a while.
	gap := 3 * time.Hour
	if next != nil {
		gap = next.Window.Start.Sub(t)
	}
	if gap > 90*time.Minute && sh.rng.Float64() > 0.55*min(in.activity, 1.4) {
		st.spot, st.spotUntil = offCampus, t.Add(time.Duration(30+sh.rng.IntN(60))*time.Minute)
		return offCampus
	}
	st.spot = sh.pickSpot(in, t)
	stay := time.Duration(30+sh.rng.IntN(90)) * time.Minute
	if r, err := sh.campus.Room(st.spot); err == nil && sh.deadZone(r) {
		stay = time.Duration(10+sh.rng.IntN(10)) * time.Minute // gives up on the Wi-Fi and moves
	}
	if next != nil {
		stay = min(stay, next.Window.Start.Sub(t)-15*time.Minute)
	}
	st.spotUntil = t.Add(max(stay, 10*time.Minute))
	return st.spot
}

// pickSpot chooses a study space by weighted sampling, skipping rooms with
// a class soon and rooms that were full last tick. Students do not know
// which rooms have bad Wi-Fi: that is exactly the problem CampusPulse solves.
func (sh *shard) pickSpot(in shardInput, t time.Time) campus.RoomID {
	var total float64
	for _, s := range sh.spots {
		total += s.weight
	}
	for attempt := 0; attempt < 8; attempt++ {
		x := sh.rng.Float64() * total
		for _, s := range sh.spots {
			if x -= s.weight; x > 0 {
				continue
			}
			r := s.room
			if in.schedule.Busy(r.ID, t, t.Add(40*time.Minute)) {
				break
			}
			if !sh.hasSeat(r, in) {
				break
			}
			// Newcomers who do pick it often turn back once they see the
			// signal (problem 3 in the proposal).
			if sh.deadZone(r) && sh.rng.Float64() < 0.75 {
				break
			}
			sh.claimed[r.ID]++
			return r.ID
		}
	}
	// Nowhere free: hang around a lobby.
	b := sh.campus.Buildings[sh.rng.IntN(len(sh.campus.Buildings))]
	return b.Floors[0].Lobby.ID
}

// deadZone reports whether the room's own AP has weak coverage. This is
// simulator ground truth used only to model student behaviour.
func (sh *shard) deadZone(r *campus.Room) bool {
	ap, err := sh.campus.AccessPoint(r.PrimaryAP)
	return err == nil && ap.BaseSignal < -80
}

// hasSeat reports whether this shard may send one more student to r this
// tick. Seats already taken or promised (people in the room or on their
// way) are counted from last tick; the seats left are split evenly between
// shards, so shards that cannot see each other still never overfill a room.
func (sh *shard) hasSeat(r *campus.Room, in shardInput) bool {
	limit := int(0.95 * float64(r.Capacity))
	taken := 0
	if in.prev != nil {
		taken = int(max(in.prev.Rooms[r.ID], in.prev.heading[r.ID]))
	}
	free := limit - taken
	if free <= 0 {
		return false
	}
	// Split the free seats exactly: every shard gets free/n, and the first
	// free%n shards get one more.
	n := max(in.shards, 1)
	share := free / n
	if sh.id < free%n {
		share++
	}
	return sh.claimed[r.ID] < share
}

// route plans the legs from the current position to target: through the
// floor lobby, down to the ground floor, outdoors between buildings, and up
// to the destination floor. Lobby dwell grows with how crowded the lobby was
// last tick, which models queues at lifts and stairs during changeovers.
func (sh *shard) route(from campus.RoomID, outdoors bool, to campus.RoomID, in shardInput) []leg {
	t := in.at
	var legs []leg
	add := func(room campus.RoomID, out bool, d time.Duration) {
		if n := len(legs); n > 0 && legs[n-1].room == room && legs[n-1].outdoors == out {
			legs[n-1].until = legs[n-1].until.Add(d)
			return
		}
		start := t
		if n := len(legs); n > 0 {
			start = legs[n-1].until
		}
		legs = append(legs, leg{room: room, outdoors: out, until: start.Add(d)})
	}
	lobbyDwell := func(lobby campus.RoomID) time.Duration {
		base := 90*time.Second + time.Duration(sh.rng.IntN(90))*time.Second
		if in.prev != nil {
			// Roughly one extra minute per 30 people already queuing.
			base += time.Duration(float64(in.prev.Rooms[lobby])/30*60) * time.Second
		}
		return min(base, 14*time.Minute)
	}

	src, _ := sh.campus.Room(from)
	dst, _ := sh.campus.Room(to)
	walk := func() time.Duration { return time.Duration(60+sh.rng.IntN(120)) * time.Second }

	// Leave the current building if needed.
	if src != nil && (dst == nil || src.BuildingID != dst.BuildingID) {
		b, _ := sh.campus.Building(src.BuildingID)
		if src.Level != 1 || !src.IsCirculation() {
			add(b.Floors[src.Level-1].Lobby.ID, false, lobbyDwell(b.Floors[src.Level-1].Lobby.ID))
		}
		if src.Level != 1 {
			add(b.Floors[0].Lobby.ID, false, lobbyDwell(b.Floors[0].Lobby.ID))
		}
	}
	if dst == nil {
		if src != nil || outdoors {
			add(offCampus, true, walk())
		}
		return legs
	}
	b, _ := sh.campus.Building(dst.BuildingID)
	switch {
	case src == nil || src.BuildingID != dst.BuildingID:
		add(offCampus, true, walk())
		add(b.Floors[0].Lobby.ID, false, lobbyDwell(b.Floors[0].Lobby.ID))
		if dst.Level != 1 {
			add(b.Floors[dst.Level-1].Lobby.ID, false, lobbyDwell(b.Floors[dst.Level-1].Lobby.ID)/2)
		}
	case src.Level != dst.Level:
		add(b.Floors[src.Level-1].Lobby.ID, false, lobbyDwell(b.Floors[src.Level-1].Lobby.ID))
		add(b.Floors[dst.Level-1].Lobby.ID, false, lobbyDwell(b.Floors[dst.Level-1].Lobby.ID)/2)
	case src.ID != dst.ID && !dst.IsCirculation() && !src.IsCirculation():
		add(b.Floors[dst.Level-1].Lobby.ID, false, 45*time.Second)
	}
	// The final leg is the destination itself, so drop a trailing lobby leg
	// that is the destination.
	if n := len(legs); n > 0 && legs[n-1].room == to && !legs[n-1].outdoors {
		legs = legs[:n-1]
	}
	return legs
}
