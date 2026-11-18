package campus

import (
	"errors"
	"testing"
)

func TestGWUCampusShape(t *testing.T) {
	c := NewGWUCampus()
	if got := len(c.Buildings); got != 3 {
		t.Fatalf("buildings = %d, want 3", got)
	}
	rooms := 0
	styles := map[string]bool{}
	for _, b := range c.Buildings {
		styles[b.Style] = true
		if len(b.Floors) != FloorCount {
			t.Errorf("%s floors = %d, want %d", b.ID, len(b.Floors), FloorCount)
		}
		for _, f := range b.Floors {
			if len(f.Rooms) != RoomsPerFloor {
				t.Errorf("%s rooms = %d, want %d", f.ID, len(f.Rooms), RoomsPerFloor)
			}
			if f.Lobby == nil {
				t.Errorf("%s has no lobby", f.ID)
			}
			rooms += len(f.Rooms)
		}
	}
	if rooms != 36 {
		t.Errorf("rooms = %d, want 36", rooms)
	}
	if len(styles) != 3 {
		t.Errorf("buildings should have distinct styles, got %v", styles)
	}
}

func TestIDsUniqueAndIndexed(t *testing.T) {
	c := NewGWUCampus()
	seen := map[RoomID]bool{}
	for _, r := range c.AllRooms() {
		if seen[r.ID] {
			t.Fatalf("duplicate room id %s", r.ID)
		}
		seen[r.ID] = true
		got, err := c.Room(r.ID)
		if err != nil || got != r {
			t.Fatalf("Room(%s) = %v, %v", r.ID, got, err)
		}
		if _, err := c.AccessPoint(r.PrimaryAP); err != nil {
			t.Errorf("room %s primary AP: %v", r.ID, err)
		}
	}
	for _, ap := range c.AllAccessPoints() {
		if len(c.RoomsServedBy(ap.ID)) == 0 {
			t.Errorf("AP %s serves no rooms", ap.ID)
		}
	}
}

func TestDemoScenarioRoomsExist(t *testing.T) {
	c := NewGWUCampus()
	r, err := c.Room("bell-101")
	if err != nil {
		t.Fatal(err)
	}
	if r.Capacity != 300 || r.Type != RoomLectureHall {
		t.Errorf("bell-101 = %d seats, %v; want 300-seat lecture hall", r.Capacity, r.Type)
	}
	east, err := c.Room("bell-203")
	if err != nil {
		t.Fatal(err)
	}
	ap, _ := c.AccessPoint(east.PrimaryAP)
	if east.Wing != WingEast || ap.BaseSignal > -80 {
		t.Errorf("bell-203 should be an east-wing room with weak coverage, got wing %s signal %v", east.Wing, ap.BaseSignal)
	}
}

func TestNotFoundErrors(t *testing.T) {
	c := NewGWUCampus()
	_, err := c.Building("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(%v, ErrNotFound) = false", err)
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) || nf.Kind != "building" {
		t.Fatalf("errors.As gave %+v", nf)
	}
}

func TestLevelForRatio(t *testing.T) {
	tests := []struct {
		ratio float64
		want  OccupancyLevel
	}{
		{0, OccupancyLow}, {0.39, OccupancyLow}, {0.40, OccupancyModerate},
		{0.64, OccupancyModerate}, {0.65, OccupancyHigh}, {0.84, OccupancyHigh},
		{0.85, OccupancyVeryHigh}, {1.3, OccupancyVeryHigh},
	}
	for _, tt := range tests {
		if got := LevelForRatio(tt.ratio); got != tt.want {
			t.Errorf("LevelForRatio(%v) = %v, want %v", tt.ratio, got, tt.want)
		}
	}
}

func TestParseRoomType(t *testing.T) {
	for _, rt := range []RoomType{RoomClassroom, RoomLectureHall, RoomStudyRoom, RoomLab, RoomMeeting, RoomLibraryArea, RoomLobby} {
		got, err := ParseRoomType(rt.String())
		if err != nil || got != rt {
			t.Errorf("round trip %v: got %v, %v", rt, got, err)
		}
	}
	if _, err := ParseRoomType("ballroom"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown type should be ErrInvalidInput, got %v", err)
	}
}

func TestValidationErrorsUnwrap(t *testing.T) {
	var none ValidationErrors
	if none.OrNil() != nil {
		t.Fatal("empty ValidationErrors must become a nil error")
	}
	errs := ValidationErrors{{Field: "a"}, {Field: "b"}}
	err := errs.OrNil()
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatal("multi-error should match ErrInvalidInput")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "a" {
		t.Fatalf("errors.As = %+v", ve)
	}
}
