package finder

import (
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

var (
	c   = campus.NewGWUCampus()
	now = time.Date(2026, 11, 18, 13, 40, 0, 0, time.UTC)
)

func cand(id campus.RoomID, people campus.OccupancyCount, wifi campus.WiFiQuality, freeFor time.Duration) Candidate {
	r, _ := c.Room(id)
	ratio := float64(people) / float64(r.Capacity)
	cd := Candidate{Room: r, Building: string(r.BuildingID), WiFi: wifi,
		Estimate:   campus.OccupancyEstimate{Count: campus.PublicCount{Value: people, Suppressed: people < 10}, Ratio: ratio, Level: campus.LevelForRatio(ratio), Capacity: r.Capacity},
		Forecast60: campus.ForecastPoint{Ratio: ratio, Level: campus.LevelForRatio(ratio)}}
	if freeFor > 0 {
		cd.FreeUntil = now.Add(freeFor)
	}
	return cd
}

func TestDeadZoneNeverRecommendedWithoutWarning(t *testing.T) {
	cands := []Candidate{
		cand("bell-203", 3, campus.WiFiPoor, 0),      // empty, but a dead zone
		cand("bell-301", 4, campus.WiFiExcellent, 0), // empty, strong Wi-Fi
		cand("gelman-2a", 85, campus.WiFiGood, 0),    // busy library floor
	}
	// Needs reliable Wi-Fi: the dead zone is excluded outright.
	for _, r := range Recommend(cands, Query{Now: now, NeedWiFi: true}) {
		if r.RoomID == "bell-203" {
			t.Fatal("dead zone recommended to someone who needs Wi-Fi")
		}
	}
	// Wi-Fi optional: it may appear, but always with a warning.
	recs := Recommend(cands, Query{Now: now})
	if recs[0].RoomID != "bell-301" {
		t.Errorf("best pick = %s, want bell-301", recs[0].RoomID)
	}
	for _, r := range recs {
		if r.RoomID == "bell-203" && r.Warning == "" {
			t.Fatal("dead zone shown without a warning")
		}
	}
}

func TestFilters(t *testing.T) {
	lib := campus.RoomLibraryArea
	cands := []Candidate{
		cand("bell-301", 4, campus.WiFiExcellent, 30*time.Minute),
		cand("gelman-1a", 40, campus.WiFiGood, 0),
		cand("seh-2300", 2, campus.WiFiGood, 0),
	}
	tests := []struct {
		name string
		q    Query
		want int
	}{
		{"everything", Query{}, 3},
		{"needs 90 minutes free", Query{For: 90 * time.Minute}, 2},
		{"library only", Query{Type: &lib}, 1},
		{"Gelman only", Query{Building: "gelman"}, 1},
		{"needs 50 seats", Query{MinFree: 50}, 1},
	}
	for _, tt := range tests {
		tt.q.Now = now
		if got := len(Recommend(cands, tt.q)); got != tt.want {
			t.Errorf("%s: %d results, want %d", tt.name, got, tt.want)
		}
	}
}

func TestBusyRoomsAndClassesExcluded(t *testing.T) {
	full := cand("bell-302", 16, campus.WiFiGood, 0)
	inClass := cand("bell-102", 0, campus.WiFiGood, 0)
	inClass.InClass = true
	if n := len(Recommend([]Candidate{full, inClass}, Query{Now: now})); n != 0 {
		t.Fatalf("got %d recommendations, want 0", n)
	}
}
