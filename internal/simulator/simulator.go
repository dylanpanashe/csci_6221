// Package simulator generates a synthetic GWU campus day: a timetable,
// students moving between rooms, the Wi-Fi observations their devices
// produce, AP telemetry, and AP faults.
//
// Concurrency layout (one tick):
//
//	loop ──fan-out──► student shard 1..N ──fan-in──► loop (merge ground truth)
//	loop ──fan-out──► building zone 1..3 ──fan-in──► loop (observations, telemetry)
//	loop ──► Feeds.Observations / Feeds.Telemetry / Feeds.Frames ──► pipeline
//
// Each shard owns a slice of students and each zone owns one building's APs.
// They share nothing mutable, so the simulation needs no locks. The only
// shared state, the current schedule and the latest ground truth, is
// published through atomic pointers to immutable snapshots.
package simulator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/simclock"
)

// Compile-time check: the simulator is a campus data source.
var _ campus.CampusSource = (*Simulator)(nil)

// ErrNotRunning is returned by control methods when Run is not active.
var ErrNotRunning = errors.New("simulator: not running")

// Config tunes the simulation.
type Config struct {
	Population int
	Seed       uint64
	Shards     int           // student shards (goroutines)
	ChunkSize  int           // observations per batch sent to the pipeline
	Tick       time.Duration // real time between ticks
	StartHour  int
	Location   *time.Location
	FailRate   float64 // spontaneous AP failures per AP per simulated hour
	Demo       bool    // start at the Bell Hall 1:40 PM scenario
}

// Truth is simulator ground truth for one tick. It exists only so the
// accuracy evaluator can score the estimates. It is never published.
type Truth struct {
	Seq        uint64
	At         time.Time
	Rooms      map[campus.RoomID]campus.OccupancyCount
	Outdoors   int
	OffCampus  int
	OfflineAPs map[campus.AccessPointID]time.Time // AP -> when it failed
	Faults     []FaultRecord                      // recent outage history
	APLoad     map[campus.AccessPointID]float64   // associated clients / AP capacity
	DeadZones  map[campus.AccessPointID]bool      // APs planted with weak coverage

	heading map[campus.RoomID]campus.OccupancyCount // students on their way to each room (simulator-internal)
}

// Simulator implements campus.CampusSource.
type Simulator struct {
	cfg    Config
	campus *campus.Campus
	clock  *simclock.SimClock
	log    *slog.Logger

	schedule atomic.Pointer[Schedule]
	truth    atomic.Pointer[Truth]
	frames   atomic.Uint64
	emitted  atomic.Int64
	running  atomic.Bool

	commands chan command

	// Everything below is owned by the Run goroutine.
	students  []*student
	shards    []*shard
	zones     []*zone
	activity  float64
	plans     []*StudentPlan
	newDay    bool
	teleport  bool // place students at their targets instantly (start-up, reset, demo)
	reset     bool
	pendingAP map[campus.BuildingID][]apCommand
	lastTick  time.Time
	offline   map[campus.AccessPointID]time.Time
	deadZones map[campus.AccessPointID]bool // immutable after New
	faults    []FaultRecord
	rng       *rand.Rand
}

// New builds a simulator. Nothing runs until Run is called.
func New(cfg Config, c *campus.Campus, clk *simclock.SimClock, log *slog.Logger) (*Simulator, error) {
	if cfg.Population < 1 {
		return nil, &campus.ValidationError{Field: "population", Value: cfg.Population, Reason: "must be positive"}
	}
	if cfg.Shards < 1 {
		cfg.Shards = 4
	}
	if cfg.ChunkSize < 1 {
		cfg.ChunkSize = 512
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 2 * time.Second
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.Seed == 0 {
		cfg.Seed = uint64(time.Now().UnixNano())
	}
	if cfg.FailRate == 0 {
		cfg.FailRate = 0.004
	}
	s := &Simulator{cfg: cfg, campus: c, clock: clk, log: log, commands: make(chan command),
		activity: 1, offline: map[campus.AccessPointID]time.Time{}, pendingAP: map[campus.BuildingID][]apCommand{}}
	s.deadZones = PlantedDeadZones(c)
	s.init()
	return s, nil
}

// init (re)creates the population, shards, zones and the day's schedule.
func (s *Simulator) init() {
	s.rng = rand.New(rand.NewPCG(s.cfg.Seed, 0xC0FFEE))
	s.students = newStudents(s.cfg.Population, s.rng)
	s.shards = s.shards[:0]
	per := (len(s.students) + s.cfg.Shards - 1) / s.cfg.Shards
	for i := 0; i < s.cfg.Shards; i++ {
		lo, hi := i*per, min((i+1)*per, len(s.students))
		if lo >= hi {
			break
		}
		s.shards = append(s.shards, newShard(i, s.campus, s.students[lo:hi], s.cfg.Seed+uint64(i)))
	}
	s.zones = s.zones[:0]
	for _, b := range s.campus.Buildings {
		s.zones = append(s.zones, newZone(s.campus, b, s.cfg.Seed, s.cfg.FailRate))
	}
	if s.cfg.Demo {
		s.clock.JumpTo(simclock.DayAt(s.clock.Now(), s.cfg.Location, 13, 40))
	}
	s.startDay(s.clock.Now())
	s.teleport = true
	s.truth.Store(&Truth{Rooms: map[campus.RoomID]campus.OccupancyCount{}, OfflineAPs: map[campus.AccessPointID]time.Time{}})
}

func (s *Simulator) startDay(now time.Time) {
	day := simclock.DayAt(now, s.cfg.Location, 0, 0)
	sched := BuildSchedule(s.campus, day, rand.New(rand.NewPCG(s.cfg.Seed, uint64(day.YearDay()))))
	s.schedule.Store(sched)
	s.plans = buildPlans(sched, len(s.students), s.rng)
	s.newDay = true
}

// PlantedDeadZones lists the APs the campus layout gave weak coverage. This
// is ground truth for scoring dead-zone detection; the diagnostics engine
// never sees it.
func PlantedDeadZones(c *campus.Campus) map[campus.AccessPointID]bool {
	out := map[campus.AccessPointID]bool{}
	for _, ap := range c.AllAccessPoints() {
		if ap.BaseSignal < -80 {
			out[ap.ID] = true
		}
	}
	return out
}

// Schedule returns the current timetable (an immutable snapshot).
func (s *Simulator) Schedule() *Schedule { return s.schedule.Load() }

// Truth returns the latest ground truth snapshot. For accuracy evaluation only.
func (s *Simulator) Truth() *Truth { return s.truth.Load() }

// Emitted is the total number of observations produced so far.
func (s *Simulator) Emitted() int64 { return s.emitted.Load() }

// Status reports whether the simulator is running and where its clock is.
func (s *Simulator) Status() Status {
	return Status{Running: s.running.Load(), Paused: s.clock.Paused(), Speed: s.clock.Speed(), SimTime: s.clock.Now(), Ticks: s.frames.Load()}
}

// Status is a read-only summary for the API.
type Status struct {
	Running bool      `json:"running"`
	Paused  bool      `json:"paused"`
	Speed   float64   `json:"speed"`
	SimTime time.Time `json:"sim_time"`
	Ticks   uint64    `json:"ticks"`
}

// Run drives the simulation until ctx is cancelled. It never closes the
// Feeds channels: the caller who made them does that after Run returns.
func (s *Simulator) Run(ctx context.Context, out campus.Feeds) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("simulator: already running")
	}
	defer s.running.Store(false)

	// Start the shard and zone workers. Each gets its own unbuffered input
	// channel; results come back on one shared channel per kind (fan-in).
	shardIn := make([]chan shardInput, len(s.shards))
	shardOut := make(chan shardOutput, len(s.shards))
	zoneIn := make([]chan zoneInput, len(s.zones))
	zoneOut := make(chan zoneOutput, len(s.zones))
	var wg sync.WaitGroup
	for i, sh := range s.shards {
		shardIn[i] = make(chan shardInput)
		wg.Add(1)
		go func() { defer wg.Done(); sh.run(shardIn[i], shardOut) }()
	}
	for i, z := range s.zones {
		zoneIn[i] = make(chan zoneInput)
		wg.Add(1)
		go func() { defer wg.Done(); z.run(zoneIn[i], zoneOut) }()
	}
	// On exit: close every worker input, then wait for the workers.
	defer func() {
		for _, c := range shardIn {
			close(c)
		}
		for _, c := range zoneIn {
			close(c)
		}
		wg.Wait()
	}()

	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	s.lastTick = s.clock.Now()
	s.log.Info("simulator started", "students", len(s.students), "shards", len(s.shards), "zones", len(s.zones), "sim_time", s.lastTick.Format("Mon 15:04"))

	for {
		select {
		case <-ctx.Done():
			return nil
		case cmd := <-s.commands:
			cmd.reply <- s.apply(cmd)
		case <-ticker.C:
			if err := s.tick(ctx, out, shardIn, shardOut, zoneIn, zoneOut); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("simulator: tick: %w", err)
			}
		}
	}
}

// tick runs one simulation step and emits its output.
func (s *Simulator) tick(ctx context.Context, out campus.Feeds, shardIn []chan shardInput, shardOut chan shardOutput,
	zoneIn []chan zoneInput, zoneOut chan zoneOutput) error {

	seq := s.frames.Add(1)
	now := s.clock.Now()
	if s.clock.Paused() {
		return send(ctx, out.Frames, s.frame(seq, now, nil, nil, 0))
	}

	// Skip the night: after 10 PM jump to 7:30 the next morning.
	if h := now.In(s.cfg.Location).Hour(); h >= 22 || h < 7 {
		next := simclock.DayAt(now, s.cfg.Location, 7, 30)
		if h >= 22 {
			next = next.AddDate(0, 0, 1)
		}
		s.clock.JumpTo(next)
		now = next
	}
	if !simclock.DayAt(now, s.cfg.Location, 0, 0).Equal(s.schedule.Load().Day) {
		s.startDay(now)
	}
	step := now.Sub(s.lastTick)
	if step < 0 || step > time.Hour {
		step = 0 // clock jumped; do not count it as elapsed time
	}
	sched := s.schedule.Load()
	transitions := sched.Transitions(s.lastTick, now)
	s.lastTick = now

	// Fan out to student shards.
	prev := s.truth.Load()
	var plans []*StudentPlan
	if s.newDay {
		plans, s.newDay = s.plans, false
	}
	for _, c := range shardIn {
		select {
		case c <- shardInput{seq: seq, at: now, schedule: sched, plans: plans, prev: prev, activity: s.activity, teleport: s.teleport, shards: len(s.shards)}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.teleport = false
	// Fan in and merge.
	truth := &Truth{Seq: seq, At: now, Rooms: make(map[campus.RoomID]campus.OccupancyCount, 64),
		APLoad: map[campus.AccessPointID]float64{}, DeadZones: s.deadZones,
		heading: make(map[campus.RoomID]campus.OccupancyCount, 64)}
	byBuilding := map[campus.BuildingID][]presence{}
	flows := map[flowKey]campus.OccupancyCount{}
	onCampus := 0
	for range shardIn {
		var r shardOutput
		select {
		case r = <-shardOut:
		case <-ctx.Done():
			return ctx.Err()
		}
		for room, n := range r.counts {
			truth.Rooms[room] += n
			onCampus += int(n)
		}
		truth.Outdoors += r.outdoors
		for _, p := range r.present {
			rm, err := s.campus.Room(p.room)
			if err == nil {
				byBuilding[rm.BuildingID] = append(byBuilding[rm.BuildingID], p)
			}
		}
		for k, n := range r.flows {
			flows[k] += n
		}
		for room, n := range r.heading {
			truth.heading[room] += n
		}
	}
	truth.OffCampus = len(s.students) - onCampus - truth.Outdoors

	// Fan out to building zones.
	reset := s.reset
	s.reset = false
	for i, c := range zoneIn {
		b := s.zones[i].building.ID
		select {
		case c <- zoneInput{seq: seq, at: now, step: step, present: byBuilding[b], activity: s.activity, commands: s.pendingAP[b], reset: reset}:
		case <-ctx.Done():
			return ctx.Err()
		}
		delete(s.pendingAP, b)
	}
	var observations []campus.WiFiObservation
	tel := campus.TelemetryBatch{Seq: seq, At: now}
	for range zoneIn {
		var z zoneOutput
		select {
		case z = <-zoneOut:
		case <-ctx.Done():
			return ctx.Err()
		}
		observations = append(observations, z.obs...)
		tel.Samples = append(tel.Samples, z.telemetry...)
		maps.Copy(truth.APLoad, z.load)
		s.recordFaults(z)
	}
	truth.OfflineAPs = maps.Clone(s.offline)
	truth.Faults = append([]FaultRecord(nil), s.faults...)
	s.truth.Store(truth)

	// Emit observations in chunks. Every chunk carries the tick's total so
	// the aggregator knows when the tick is complete.
	total := (len(observations) + s.cfg.ChunkSize - 1) / s.cfg.ChunkSize
	for i := 0; i < total; i++ {
		lo, hi := i*s.cfg.ChunkSize, min((i+1)*s.cfg.ChunkSize, len(observations))
		batch := campus.ObservationBatch{Seq: seq, At: now, Index: i, Total: total, Observations: observations[lo:hi]}
		if err := send(ctx, out.Observations, batch); err != nil {
			return err
		}
	}
	if total == 0 { // still mark the tick so downstream stages advance
		if err := send(ctx, out.Observations, campus.ObservationBatch{Seq: seq, At: now, Total: 1}); err != nil {
			return err
		}
	}
	s.emitted.Add(int64(len(observations)))
	if err := send(ctx, out.Telemetry, tel); err != nil {
		return err
	}
	return send(ctx, out.Frames, s.frame(seq, now, flows, transitions, len(observations)))
}

func (s *Simulator) recordFaults(z zoneOutput) {
	for _, f := range z.started {
		s.offline[f.AccessPoint] = f.Start
		s.faults = append(s.faults, f)
		s.log.Info("simulated AP failure", "ap", f.AccessPoint, "manual", f.Manual, "sim_time", f.Start.Format("15:04"))
	}
	for _, f := range z.ended {
		delete(s.offline, f.AccessPoint)
		for i := len(s.faults) - 1; i >= 0; i-- {
			if s.faults[i].AccessPoint == f.AccessPoint && s.faults[i].End == nil {
				s.faults[i].End = f.End
				break
			}
		}
		s.log.Info("simulated AP recovered", "ap", f.AccessPoint)
	}
	if len(s.faults) > 50 {
		s.faults = s.faults[len(s.faults)-50:]
	}
}

func (s *Simulator) frame(seq uint64, now time.Time, flows map[flowKey]campus.OccupancyCount, tr []campus.TimetableTransition, observed int) campus.SimFrame {
	f := campus.SimFrame{Seq: seq, At: now, Speed: s.clock.Speed(), Paused: s.clock.Paused(), Activity: s.activity,
		Transitions: tr, Observed: observed}
	for k, n := range flows {
		f.Flows = append(f.Flows, campus.MovementFlow{From: k.from, To: k.to, Outdoors: k.outdoors, Count: n})
	}
	return f
}

// send delivers v unless ctx is cancelled first. A nil channel is skipped,
// which lets tests leave feeds they do not care about unset.
func send[T any](ctx context.Context, ch chan<- T, v T) error {
	if ch == nil {
		return nil
	}
	select {
	case ch <- v:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
