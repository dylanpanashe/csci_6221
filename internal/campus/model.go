package campus

import (
	"time"
)

// Ownership rule for this file
//
// Campus, Building, Floor, Room and AccessPoint describe the physical campus.
// They are built once at start-up by NewGWUCampus and never mutated after
// that, so any number of goroutines can read them without locks.
//
// Everything that changes over time (occupancy, AP telemetry, alerts) lives in
// separate value types below and in the state store, which owns its own
// locking. Keeping the two apart is what lets the pipeline run without a
// global mutex.

// Location is shared by anything that sits somewhere in a building. It is
// embedded (composition), so Room and AccessPoint both get its fields and
// methods without any inheritance.
type Location struct {
	BuildingID BuildingID `json:"building_id"`
	FloorID    FloorID    `json:"floor_id"`
	Level      int        `json:"level"` // 1-based floor number
	Wing       Wing       `json:"wing"`
	Position   Point      `json:"position"` // campus coordinates, metres
}

// SameFloor reports whether two locations are on the same floor.
func (l Location) SameFloor(o Location) bool { return l.FloorID == o.FloorID }

// Room is a bookable or usable space.
type Room struct {
	Location
	ID          RoomID          `json:"id"`
	Name        string          `json:"name"`
	Number      string          `json:"number"` // e.g. "101"
	Type        RoomType        `json:"type"`
	Capacity    OccupancyCount  `json:"capacity"`
	Width       float64         `json:"width"` // metres, used by the 3D floor plan
	Depth       float64         `json:"depth"`
	PrimaryAP   AccessPointID   `json:"primary_ap"`
	NeighbourAP []AccessPointID `json:"neighbour_aps,omitempty"` // APs whose signal leaks in
}

// IsCirculation reports whether this is a lobby/stair/lift area rather than
// a room people book.
func (r *Room) IsCirculation() bool { return r.Type == RoomLobby }

// AccessPoint is the static description of a Wi-Fi AP.
type AccessPoint struct {
	Location
	ID         AccessPointID `json:"id"`
	Label      string        `json:"label"`
	Channel    int           `json:"channel"`
	MaxClients DeviceCount   `json:"max_clients"`
	// BaseSignal is the typical RSSI a device sees in the AP's own room.
	// The simulator plants dead zones by giving an AP's coverage area a very
	// weak base signal; the diagnostics engine has to discover that on its own.
	BaseSignal SignalStrength `json:"-"`
}

// Floor groups rooms and APs on one storey.
type Floor struct {
	ID         FloorID        `json:"id"`
	BuildingID BuildingID     `json:"building_id"`
	Level      int            `json:"level"`
	Elevation  float64        `json:"elevation"` // metres above ground
	Height     float64        `json:"height"`
	Rooms      []*Room        `json:"rooms"`
	Lobby      *Room          `json:"lobby"` // stairs, lifts, corridor
	APs        []*AccessPoint `json:"access_points"`
}

// Capacity is the total seated capacity of the floor's rooms (lobby excluded).
func (f *Floor) Capacity() OccupancyCount {
	var total OccupancyCount
	for _, r := range f.Rooms {
		total += r.Capacity
	}
	return total
}

// Building is one of the campus buildings.
type Building struct {
	ID          BuildingID `json:"id"`
	Name        string     `json:"name"`
	ShortName   string     `json:"short_name"`
	Style       string     `json:"style"` // hint for the 3D renderer
	Footprint   Footprint  `json:"footprint"`
	FloorHeight float64    `json:"floor_height"`
	Floors      []*Floor   `json:"floors"`
	Entrances   []Point    `json:"entrances"`
}

// Capacity is the total room capacity across all floors.
func (b *Building) Capacity() OccupancyCount {
	var total OccupancyCount
	for _, f := range b.Floors {
		total += f.Capacity()
	}
	return total
}

// Floor returns the floor at the given 1-based level.
func (b *Building) Floor(level int) (*Floor, error) {
	if level < 1 || level > len(b.Floors) {
		return nil, &NotFoundError{Kind: "floor", ID: string(b.ID) + "/" + itoa(level)}
	}
	return b.Floors[level-1], nil
}

// Campus is the whole site, with indexes built once for fast lookups.
type Campus struct {
	Name      string      `json:"name"`
	Buildings []*Building `json:"buildings"`
	Paths     [][]Point   `json:"paths"` // walkways between entrances, for the 3D scene

	buildings map[BuildingID]*Building
	floors    map[FloorID]*Floor
	rooms     map[RoomID]*Room
	aps       map[AccessPointID]*AccessPoint
	apRooms   map[AccessPointID][]*Room
}

// index builds the lookup maps. Called once by the constructor.
func (c *Campus) index() {
	c.buildings = make(map[BuildingID]*Building)
	c.floors = make(map[FloorID]*Floor)
	c.rooms = make(map[RoomID]*Room)
	c.aps = make(map[AccessPointID]*AccessPoint)
	c.apRooms = make(map[AccessPointID][]*Room)
	for _, b := range c.Buildings {
		c.buildings[b.ID] = b
		for _, f := range b.Floors {
			c.floors[f.ID] = f
			for _, r := range append(f.Rooms[:len(f.Rooms):len(f.Rooms)], f.Lobby) {
				c.rooms[r.ID] = r
				c.apRooms[r.PrimaryAP] = append(c.apRooms[r.PrimaryAP], r)
			}
			for _, ap := range f.APs {
				c.aps[ap.ID] = ap
			}
		}
	}
}

// Building looks up a building by ID.
func (c *Campus) Building(id BuildingID) (*Building, error) {
	if b, ok := c.buildings[id]; ok {
		return b, nil
	}
	return nil, &NotFoundError{Kind: "building", ID: string(id)}
}

// FloorByID looks up a floor.
func (c *Campus) FloorByID(id FloorID) (*Floor, error) {
	if f, ok := c.floors[id]; ok {
		return f, nil
	}
	return nil, &NotFoundError{Kind: "floor", ID: string(id)}
}

// Room looks up a room (including lobbies).
func (c *Campus) Room(id RoomID) (*Room, error) {
	if r, ok := c.rooms[id]; ok {
		return r, nil
	}
	return nil, &NotFoundError{Kind: "room", ID: string(id)}
}

// AccessPoint looks up an AP.
func (c *Campus) AccessPoint(id AccessPointID) (*AccessPoint, error) {
	if ap, ok := c.aps[id]; ok {
		return ap, nil
	}
	return nil, &NotFoundError{Kind: "access point", ID: string(id)}
}

// RoomsServedBy lists the rooms whose primary AP is id. Used to work out
// which rooms an AP failure affects.
func (c *Campus) RoomsServedBy(id AccessPointID) []*Room {
	return c.apRooms[id]
}

// AllRooms returns every room on campus, lobbies included, in a stable order.
func (c *Campus) AllRooms() []*Room {
	out := make([]*Room, 0, len(c.rooms))
	for _, b := range c.Buildings {
		for _, f := range b.Floors {
			out = append(out, f.Rooms...)
			out = append(out, f.Lobby)
		}
	}
	return out
}

// AllAccessPoints returns every AP in a stable order.
func (c *Campus) AllAccessPoints() []*AccessPoint {
	out := make([]*AccessPoint, 0, len(c.aps))
	for _, b := range c.Buildings {
		for _, f := range b.Floors {
			out = append(out, f.APs...)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Values that flow through the pipeline
// ---------------------------------------------------------------------------

// TimetableEvent is a scheduled class or booking.
type TimetableEvent struct {
	ID                 EventID        `json:"id"`
	RoomID             RoomID         `json:"room_id"`
	BuildingID         BuildingID     `json:"building_id"`
	Level              int            `json:"level"`
	Title              string         `json:"title"`
	Kind               EventKind      `json:"kind"`
	Window             Window         `json:"window"`
	Enrolled           OccupancyCount `json:"enrolled"`
	ExpectedAttendance OccupancyCount `json:"expected_attendance"`
	Cancelled          bool           `json:"cancelled"`
}

// ActiveAt reports whether the event is running at t.
func (e TimetableEvent) ActiveAt(t time.Time) bool {
	return !e.Cancelled && e.Window.Contains(t)
}

// WiFiObservation is one association record from an AP. DeviceHash is never
// the raw device identifier: the privacy filter replaces it before anything
// leaves the ingest stage. RawDeviceID only exists inside the simulator and
// the first pipeline stage, and is tagged so it is never serialised.
type WiFiObservation struct {
	RawDeviceID string         `json:"-"`
	DeviceHash  string         `json:"-"`
	AccessPoint AccessPointID  `json:"ap_id"`
	Signal      SignalStrength `json:"signal"`
	Connected   bool           `json:"connected"`
	At          time.Time      `json:"at"`
}

// APTelemetry is a periodic health sample from an AP.
type APTelemetry struct {
	AccessPoint AccessPointID         `json:"ap_id"`
	Clients     DeviceCount           `json:"clients"`
	Utilization UtilizationPercentage `json:"utilization"`
	AvgSignal   SignalStrength        `json:"avg_signal"`
	Latency     Latency               `json:"latency_ms"`
	PacketLoss  PacketLoss            `json:"packet_loss"`
	Online      bool                  `json:"online"`
	At          time.Time             `json:"at"`
}

// PublicCount is a count that has been through the privacy layer. Values
// under the suppression threshold are hidden and shown as "<10".
type PublicCount struct {
	Value      OccupancyCount `json:"value"` // 0 when suppressed
	Suppressed bool           `json:"suppressed"`
	Display    string         `json:"display"` // "<10" or the number
}

// OccupancyEstimate is the fused, privacy-safe estimate for a room.
type OccupancyEstimate struct {
	RoomID     RoomID         `json:"room_id"`
	Count      PublicCount    `json:"count"`
	Low        OccupancyCount `json:"low"` // range, because devices are not people
	High       OccupancyCount `json:"high"`
	Capacity   OccupancyCount `json:"capacity"`
	Ratio      float64        `json:"ratio"`
	Level      OccupancyLevel `json:"level"`
	Confidence float64        `json:"confidence"` // 0..1
	Expected   OccupancyCount `json:"expected"`   // what the timetable alone predicts
	Devices    PublicCount    `json:"devices"`
	At         time.Time      `json:"at"`
}

// Available is the estimated number of free seats.
func (e OccupancyEstimate) Available() OccupancyCount {
	if free := e.Capacity - e.Count.Value; free > 0 {
		return free
	}
	return 0
}

// ForecastPoint is the prediction for one horizon.
type ForecastPoint struct {
	Horizon    time.Duration  `json:"horizon"`
	Minutes    int            `json:"minutes"`
	At         time.Time      `json:"at"`
	Count      OccupancyCount `json:"count"`
	Ratio      float64        `json:"ratio"`
	Level      OccupancyLevel `json:"level"`
	Confidence float64        `json:"confidence"`
	Factors    []string       `json:"factors"` // plain-language reasons
}

// Forecast holds predictions for a room, floor or building.
type Forecast struct {
	Subject string          `json:"subject"` // room, floor or building ID
	Kind    string          `json:"kind"`    // "room", "floor", "building"
	Points  []ForecastPoint `json:"points"`
	At      time.Time       `json:"at"`
}

// Diagnosis is the output of one diagnostic rule for one AP.
type Diagnosis struct {
	AccessPoint AccessPointID `json:"ap_id"`
	Kind        DiagnosisKind `json:"kind"`
	Severity    Severity      `json:"severity"`
	Rule        string        `json:"rule"`
	Detail      string        `json:"detail"`
	At          time.Time     `json:"at"`
}

// Alert is something a human should look at.
type Alert struct {
	ID            AlertID       `json:"id"`
	Kind          DiagnosisKind `json:"kind"`
	Severity      Severity      `json:"severity"`
	Title         string        `json:"title"`
	Detail        string        `json:"detail"`
	BuildingID    BuildingID    `json:"building_id"`
	Level         int           `json:"level"`
	AccessPoint   AccessPointID `json:"ap_id,omitempty"`
	AffectedRooms []RoomID      `json:"affected_rooms,omitempty"`
	RaisedAt      time.Time     `json:"raised_at"`
	ResolvedAt    *time.Time    `json:"resolved_at,omitempty"`
	// DetectionDelay is how long after the (simulated) fault the alert fired.
	DetectionDelay time.Duration `json:"detection_delay,omitempty"`
}

// Active reports whether the alert is still open.
func (a *Alert) Active() bool { return a.ResolvedAt == nil }

// Resolve marks the alert closed. Pointer receiver: it changes the alert.
func (a *Alert) Resolve(at time.Time) {
	if a.ResolvedAt == nil {
		a.ResolvedAt = &at
	}
}
