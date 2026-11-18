package campus

import (
	"fmt"
	"strings"
)

// Building IDs used across the system.
const (
	BellHall      BuildingID = "bell"
	SEH           BuildingID = "seh"
	GelmanLib     BuildingID = "gelman"
	FloorCount               = 3
	RoomsPerFloor            = 4
)

// Default signal levels. A device in a well-covered room sees around -55 dBm.
// Planted dead zones sit below -80 dBm, where video calls and large downloads
// start to fail even though the AP itself is lightly loaded.
const (
	signalGood     SignalStrength = -55
	signalDeadZone SignalStrength = -84
)

// roomSpec is one row of the campus definition table below.
type roomSpec struct {
	number   string
	name     string
	kind     RoomType
	capacity OccupancyCount
}

// buildingSpec is the declarative description of a building. The campus is
// defined as data and turned into linked structs by one function, rather
// than being assembled by hand in dozens of statements.
type buildingSpec struct {
	id          BuildingID
	name, short string
	style       string
	origin      Point
	width       float64
	depth       float64
	floorHeight float64
	entrances   []Point // offsets from origin
	floors      [FloorCount][RoomsPerFloor]roomSpec
	// deadZones lists wing APs planted with weak coverage, e.g. "2E".
	deadZones []string
}

// Room slots on each floor: 0 = west-north, 1 = west-south,
// 2 = east-north, 3 = east-south. The central strip is the lobby.
var slotWings = [RoomsPerFloor]Wing{WingWest, WingWest, WingEast, WingEast}

var gwuSpecs = []buildingSpec{
	{
		id: BellHall, name: "Bell Hall", short: "Bell", style: "brick_classical",
		origin: Point{X: -62, Z: 6}, width: 42, depth: 28, floorHeight: 4.4,
		entrances: []Point{{X: 0, Z: 14}, {X: -21, Z: 0}},
		floors: [FloorCount][RoomsPerFloor]roomSpec{
			{{"101", "Lecture Hall 101", RoomLectureHall, 300}, {"102", "Classroom 102", RoomClassroom, 45}, {"103", "Classroom 103", RoomClassroom, 35}, {"104", "Meeting Room 104", RoomMeeting, 12}},
			{{"201", "Classroom 201", RoomClassroom, 50}, {"202", "Seminar Room 202", RoomClassroom, 30}, {"203", "East Wing Study Room 203", RoomStudyRoom, 16}, {"204", "Classroom 204", RoomClassroom, 40}},
			{{"301", "Study Room 301", RoomStudyRoom, 20}, {"302", "Study Room 302", RoomStudyRoom, 16}, {"303", "Classroom 303", RoomClassroom, 40}, {"304", "Meeting Room 304", RoomMeeting, 14}},
		},
		deadZones: []string{"2E"},
	},
	{
		id: SEH, name: "Science & Engineering Hall", short: "SEH", style: "glass_modern",
		origin: Point{X: 4, Z: -18}, width: 50, depth: 32, floorHeight: 5.0,
		entrances: []Point{{X: 0, Z: 16}, {X: 25, Z: 0}},
		floors: [FloorCount][RoomsPerFloor]roomSpec{
			{{"B110", "Lecture Hall B110", RoomLectureHall, 180}, {"B120", "Engineering Lab B120", RoomLab, 30}, {"B130", "Robotics Lab B130", RoomLab, 30}, {"B140", "Project Room B140", RoomMeeting, 10}},
			{{"2100", "Chemistry Lab 2100", RoomLab, 24}, {"2200", "Classroom 2200", RoomClassroom, 60}, {"2300", "Study Pod 2300", RoomStudyRoom, 18}, {"2400", "Physics Lab 2400", RoomLab, 24}},
			{{"3100", "Study Commons 3100", RoomStudyRoom, 24}, {"3200", "Classroom 3200", RoomClassroom, 48}, {"3300", "Computing Lab 3300", RoomLab, 20}, {"3400", "Meeting Room 3400", RoomMeeting, 12}},
		},
		deadZones: []string{"3W"},
	},
	{
		id: GelmanLib, name: "Gelman Library", short: "Gelman", style: "brutalist",
		origin: Point{X: 66, Z: 10}, width: 38, depth: 38, floorHeight: 4.0,
		entrances: []Point{{X: 0, Z: 19}, {X: -19, Z: 0}},
		floors: [FloorCount][RoomsPerFloor]roomSpec{
			{{"1A", "Reading Room 1A", RoomLibraryArea, 120}, {"1B", "Group Study 1B", RoomStudyRoom, 10}, {"1C", "Group Study 1C", RoomStudyRoom, 10}, {"1D", "Computer Commons 1D", RoomLibraryArea, 60}},
			{{"2A", "Collaboration Floor 2A", RoomLibraryArea, 100}, {"2B", "Group Study 2B", RoomStudyRoom, 8}, {"2C", "Group Study 2C", RoomStudyRoom, 8}, {"2D", "Group Study 2D", RoomStudyRoom, 12}},
			{{"3A", "Quiet Reading 3A", RoomLibraryArea, 80}, {"3B", "Carrel Room 3B", RoomStudyRoom, 6}, {"3C", "Carrel Room 3C", RoomStudyRoom, 6}, {"3D", "Group Study 3D", RoomStudyRoom, 10}},
		},
		deadZones: []string{"1E"},
	},
}

// NewGWUCampus builds the three-building simulated campus: 3 buildings,
// 3 floors each, 4 rooms per floor (36 rooms), plus a lobby per floor and
// three or four APs per floor.
func NewGWUCampus() *Campus {
	c := &Campus{Name: "George Washington University (Foggy Bottom) — simulated"}
	for _, spec := range gwuSpecs {
		c.Buildings = append(c.Buildings, buildBuilding(spec))
	}
	c.Paths = walkways(c.Buildings)
	c.index()
	return c
}

func buildBuilding(s buildingSpec) *Building {
	b := &Building{
		ID: s.id, Name: s.name, ShortName: s.short, Style: s.style,
		Footprint:   Footprint{Origin: s.origin, Width: s.width, Depth: s.depth},
		FloorHeight: s.floorHeight,
	}
	for _, e := range s.entrances {
		b.Entrances = append(b.Entrances, Point{X: s.origin.X + e.X, Z: s.origin.Z + e.Z})
	}

	dead := make(map[string]bool, len(s.deadZones))
	for _, d := range s.deadZones {
		dead[d] = true
	}

	// Slot geometry: wings take 40% of the width each, lobby the middle 20%.
	wingW := s.width * 0.4
	slotD := s.depth / 2
	slotCentreX := [RoomsPerFloor]float64{-s.width/2 + wingW/2, -s.width/2 + wingW/2, s.width/2 - wingW/2, s.width/2 - wingW/2}
	slotCentreZ := [RoomsPerFloor]float64{-slotD / 2, slotD / 2, -slotD / 2, slotD / 2}

	for lvl := 1; lvl <= FloorCount; lvl++ {
		fid := FloorID(fmt.Sprintf("%s-%d", s.id, lvl))
		elev := float64(lvl-1) * s.floorHeight
		f := &Floor{ID: fid, BuildingID: s.id, Level: lvl, Elevation: elev, Height: s.floorHeight}

		loc := func(w Wing, x, z float64) Location {
			return Location{BuildingID: s.id, FloorID: fid, Level: lvl, Wing: w,
				Position: Point{X: s.origin.X + x, Y: elev + 1.2, Z: s.origin.Z + z}}
		}

		// One AP per wing plus one in the lobby.
		mkAP := func(suffix string, w Wing, x float64, max DeviceCount) *AccessPoint {
			key := fmt.Sprintf("%d%s", lvl, suffix)
			sig := signalGood
			if dead[key] {
				sig = signalDeadZone
			}
			ap := &AccessPoint{
				Location:   loc(w, x, 0),
				ID:         AccessPointID(fmt.Sprintf("%s-%d-%s", s.id, lvl, strings.ToLower(suffix))),
				Label:      fmt.Sprintf("%s L%d %s", s.short, lvl, wingLabel(suffix)),
				MaxClients: max,
				BaseSignal: sig,
			}
			ap.Position.Y = elev + s.floorHeight - 0.4 // ceiling mounted
			return ap
		}
		west := mkAP("W", WingWest, -s.width/2+wingW/2, 120)
		east := mkAP("E", WingEast, s.width/2-wingW/2, 120)
		lobbyAP := mkAP("L", WingCentre, 0, 150)
		f.APs = []*AccessPoint{west, east, lobbyAP}

		for slot, rs := range s.floors[lvl-1] {
			wing := slotWings[slot]
			primary, other := west, east
			if wing == WingEast {
				primary, other = east, west
			}
			r := &Room{
				Location: loc(wing, slotCentreX[slot], slotCentreZ[slot]),
				ID:       RoomID(fmt.Sprintf("%s-%s", s.id, strings.ToLower(rs.number))),
				Name:     rs.name, Number: rs.number, Type: rs.kind, Capacity: rs.capacity,
				Width: wingW, Depth: slotD,
				PrimaryAP:   primary.ID,
				NeighbourAP: []AccessPointID{lobbyAP.ID, other.ID},
			}
			// Large lecture halls get a dedicated high-density AP.
			if rs.kind == RoomLectureHall {
				hall := mkAP("H", wing, slotCentreX[slot], 420)
				hall.Position.Z = s.origin.Z + slotCentreZ[slot]
				f.APs = append(f.APs, hall)
				r.PrimaryAP = hall.ID
				r.NeighbourAP = append(r.NeighbourAP, primary.ID)
			}
			f.Rooms = append(f.Rooms, r)
		}

		f.Lobby = &Room{
			Location:    loc(WingCentre, 0, 0),
			ID:          RoomID(fmt.Sprintf("%s-%d-lobby", s.id, lvl)),
			Name:        fmt.Sprintf("%s Floor %d Lobby, Stairs & Lifts", s.short, lvl),
			Number:      fmt.Sprintf("L%d", lvl),
			Type:        RoomLobby,
			Capacity:    OccupancyCount(s.width * s.depth * 0.2 / 1.5), // ~1.5 m² per person standing
			Width:       s.width * 0.2,
			Depth:       s.depth,
			PrimaryAP:   lobbyAP.ID,
			NeighbourAP: []AccessPointID{west.ID, east.ID},
		}
		b.Floors = append(b.Floors, f)
	}
	return b
}

func wingLabel(suffix string) string {
	switch suffix {
	case "W":
		return "West Wing"
	case "E":
		return "East Wing"
	case "H":
		return "Lecture Hall"
	default:
		return "Lobby"
	}
}

// walkways joins every building entrance to a central quad and to each other.
func walkways(bs []*Building) [][]Point {
	quad := Point{X: 2, Z: 30}
	var paths [][]Point
	for _, b := range bs {
		for _, e := range b.Entrances {
			paths = append(paths, []Point{e, quad})
		}
	}
	for i := 0; i+1 < len(bs); i++ {
		paths = append(paths, []Point{bs[i].Entrances[0], bs[i+1].Entrances[0]})
	}
	return paths
}
