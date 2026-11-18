// Package campus holds the CampusPulse domain model: the buildings, floors,
// rooms and access points that make up the simulated campus, plus the value
// types that flow through the pipeline (observations, estimates, forecasts,
// alerts).
//
// The package has no dependencies on HTTP, WebSockets or the simulator. Every
// other package depends on it, never the other way round.
package campus

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Identifier types
//
// Each ID is its own named type. A RoomID cannot be passed where a BuildingID
// is expected without an explicit conversion, so mixing them up is a compile
// error rather than a runtime bug.
// ---------------------------------------------------------------------------

type (
	BuildingID    string
	FloorID       string
	RoomID        string
	AccessPointID string
	EventID       string
	AlertID       string
)

// ---------------------------------------------------------------------------
// Measurement types
// ---------------------------------------------------------------------------

// OccupancyCount is a number of people (estimated or true).
type OccupancyCount int

// DeviceCount is a number of Wi-Fi devices. It is deliberately a different
// type from OccupancyCount: devices are not people, and the compiler makes
// you go through a calibration function to turn one into the other.
type DeviceCount int

// SignalStrength is a received signal strength in dBm (typically -30 to -95).
type SignalStrength float64

// UtilizationPercentage is channel or AP utilisation, 0 to 100.
type UtilizationPercentage float64

// Latency is round-trip latency in milliseconds.
type Latency float64

// PacketLoss is packet loss as a percentage, 0 to 100.
type PacketLoss float64

// Clamp keeps a utilisation value inside 0..100.
func (u UtilizationPercentage) Clamp() UtilizationPercentage {
	switch {
	case u < 0:
		return 0
	case u > 100:
		return 100
	default:
		return u
	}
}

// String formats a signal reading the way network engineers write it.
func (s SignalStrength) String() string { return fmt.Sprintf("%.0f dBm", float64(s)) }

// ---------------------------------------------------------------------------
// Enumerations
// ---------------------------------------------------------------------------

// RoomType describes what a room is used for.
type RoomType int

const (
	RoomClassroom RoomType = iota
	RoomLectureHall
	RoomStudyRoom
	RoomLab
	RoomMeeting
	RoomLibraryArea
	RoomLobby // shared circulation space: lobbies, stair and lift areas
)

var roomTypeNames = [...]string{"classroom", "lecture_hall", "study_room", "lab", "meeting_room", "library_area", "lobby"}

func (t RoomType) String() string {
	if int(t) < len(roomTypeNames) {
		return roomTypeNames[t]
	}
	return fmt.Sprintf("RoomType(%d)", int(t))
}

// MarshalText lets RoomType appear as a readable string in JSON.
func (t RoomType) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// ParseRoomType converts an API filter value back into a RoomType.
func ParseRoomType(s string) (RoomType, error) {
	for i, n := range roomTypeNames {
		if n == s {
			return RoomType(i), nil
		}
	}
	return 0, &ValidationError{Field: "room_type", Value: s, Reason: "unknown room type"}
}

// IsStudyFriendly reports whether students may use the room for self-study
// when no class is scheduled in it.
func (t RoomType) IsStudyFriendly() bool {
	switch t {
	case RoomStudyRoom, RoomLibraryArea, RoomClassroom, RoomMeeting:
		return true
	default:
		return false
	}
}

// OccupancyLevel is the four-colour status used across the UI.
type OccupancyLevel int

const (
	OccupancyLow      OccupancyLevel = iota // green
	OccupancyModerate                       // yellow
	OccupancyHigh                           // orange
	OccupancyVeryHigh                       // red
)

var occupancyLevelNames = [...]string{"low", "moderate", "high", "very_high"}

func (l OccupancyLevel) String() string {
	if int(l) < len(occupancyLevelNames) {
		return occupancyLevelNames[l]
	}
	return fmt.Sprintf("OccupancyLevel(%d)", int(l))
}

func (l OccupancyLevel) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// LevelForRatio maps an occupancy ratio (0..1+) onto a status level.
// It is a pure function: same input, same output, no shared state.
func LevelForRatio(ratio float64) OccupancyLevel {
	switch {
	case ratio < 0.40:
		return OccupancyLow
	case ratio < 0.65:
		return OccupancyModerate
	case ratio < 0.85:
		return OccupancyHigh
	default:
		return OccupancyVeryHigh
	}
}

// WiFiQuality is the student-facing summary of connectivity in a space.
type WiFiQuality int

const (
	WiFiExcellent WiFiQuality = iota
	WiFiGood
	WiFiFair
	WiFiPoor
	WiFiNone // dead zone or AP offline
)

var wifiQualityNames = [...]string{"excellent", "good", "fair", "poor", "none"}

func (q WiFiQuality) String() string {
	if int(q) < len(wifiQualityNames) {
		return wifiQualityNames[q]
	}
	return fmt.Sprintf("WiFiQuality(%d)", int(q))
}

func (q WiFiQuality) MarshalText() ([]byte, error) { return []byte(q.String()), nil }

// Reliable reports whether the space is fine for someone who needs the internet.
func (q WiFiQuality) Reliable() bool { return q <= WiFiGood }

// APHealth is the operational state of an access point.
type APHealth int

const (
	APHealthy APHealth = iota
	APDegraded
	APCongested
	APOffline
)

var apHealthNames = [...]string{"healthy", "degraded", "congested", "offline"}

func (h APHealth) String() string {
	if int(h) < len(apHealthNames) {
		return apHealthNames[h]
	}
	return fmt.Sprintf("APHealth(%d)", int(h))
}

func (h APHealth) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// DiagnosisKind separates the two Wi-Fi problems IT needs to tell apart.
type DiagnosisKind int

const (
	DiagnosisHealthy DiagnosisKind = iota
	DiagnosisCongestion
	DiagnosisDeadZone
	DiagnosisAPFailure
	DiagnosisDegraded
)

var diagnosisNames = [...]string{"healthy", "congestion", "dead_zone", "ap_failure", "degraded"}

func (d DiagnosisKind) String() string {
	if int(d) < len(diagnosisNames) {
		return diagnosisNames[d]
	}
	return fmt.Sprintf("DiagnosisKind(%d)", int(d))
}

func (d DiagnosisKind) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Severity orders alerts.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityCritical
)

var severityNames = [...]string{"info", "warning", "critical"}

func (s Severity) String() string {
	if int(s) < len(severityNames) {
		return severityNames[s]
	}
	return fmt.Sprintf("Severity(%d)", int(s))
}

func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// EventKind is the kind of timetable event.
type EventKind int

const (
	EventLecture EventKind = iota
	EventSeminar
	EventLabSession
	EventExam
	EventMeeting
)

var eventKindNames = [...]string{"lecture", "seminar", "lab_session", "exam", "meeting"}

func (k EventKind) String() string {
	if int(k) < len(eventKindNames) {
		return eventKindNames[k]
	}
	return fmt.Sprintf("EventKind(%d)", int(k))
}

func (k EventKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// ---------------------------------------------------------------------------
// Geometry
// ---------------------------------------------------------------------------

// Point is a position in campus metres. X runs west to east, Z north to south,
// Y is height. The frontend uses the same axes for the Three.js scene.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Footprint is the ground-plan size and position of a building.
type Footprint struct {
	Origin Point   `json:"origin"` // centre of the ground floor
	Width  float64 `json:"width"`  // along X
	Depth  float64 `json:"depth"`  // along Z
}

// Wing is the side of a floor a room sits on.
type Wing string

const (
	WingWest   Wing = "west"
	WingEast   Wing = "east"
	WingNorth  Wing = "north"
	WingSouth  Wing = "south"
	WingCentre Wing = "centre"
)

// Window is a half-open time interval [Start, End).
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool { return !t.Before(w.Start) && t.Before(w.End) }

// Overlaps reports whether two windows share any instant.
func (w Window) Overlaps(o Window) bool { return w.Start.Before(o.End) && o.Start.Before(w.End) }

// Duration is the length of the window.
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }
