// Package finder recommends study spaces. Filters are composable predicates
// and ranking is a pure scoring function, so the whole package can be
// tested without a running system.
package finder

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/fn"
)

// Candidate is everything known about one room right now.
type Candidate struct {
	Room       *campus.Room
	Building   string
	Estimate   campus.OccupancyEstimate // published (privacy-safe)
	WiFi       campus.WiFiQuality
	WiFiDetail string
	Forecast60 campus.ForecastPoint // room-level; used to rank only
	FreeUntil  time.Time            // next class start; zero = free for the rest of the day
	InClass    bool                 // a class is running now
}

// Query describes what the student wants.
type Query struct {
	Building campus.BuildingID
	Level    int
	Type     *campus.RoomType
	MinFree  int                 // seats needed
	MinWiFi  *campus.WiFiQuality // worst acceptable quality
	NeedWiFi bool                // exclude unreliable Wi-Fi instead of warning
	For      time.Duration       // how long the space is needed
	MaxLevel campus.OccupancyLevel
	Now      time.Time
	Limit    int
}

// Recommendation is a ranked result with the reasons behind it.
type Recommendation struct {
	RoomID     campus.RoomID            `json:"room_id"`
	Room       string                   `json:"room"`
	Building   string                   `json:"building"`
	BuildingID campus.BuildingID        `json:"building_id"`
	Level      int                      `json:"level"`
	Type       campus.RoomType          `json:"type"`
	Estimate   campus.OccupancyEstimate `json:"estimate"`
	Available  string                   `json:"available"` // "about 12 seats" or "a few seats"
	WiFi       campus.WiFiQuality       `json:"wifi"`
	Forecast60 campus.OccupancyLevel    `json:"forecast_60"`
	QuietUntil *time.Time               `json:"quiet_until,omitempty"`
	Score      float64                  `json:"score"`
	Reasons    []string                 `json:"reasons"`
	Warning    string                   `json:"warning,omitempty"`
}

// Filters builds the predicate list for a query. Each filter is a small
// closure; fn.And combines them.
func Filters(q Query) fn.Predicate[Candidate] {
	ps := []fn.Predicate[Candidate]{
		func(c Candidate) bool { return c.Room.Type.IsStudyFriendly() && !c.InClass },
		func(c Candidate) bool { return c.Estimate.Level <= q.MaxLevel },
	}
	if q.Building != "" {
		ps = append(ps, func(c Candidate) bool { return c.Room.BuildingID == q.Building })
	}
	if q.Level > 0 {
		ps = append(ps, func(c Candidate) bool { return c.Room.Level == q.Level })
	}
	if q.Type != nil {
		t := *q.Type
		ps = append(ps, func(c Candidate) bool { return c.Room.Type == t })
	}
	if q.MinFree > 0 {
		ps = append(ps, func(c Candidate) bool { return seatsFree(c) >= q.MinFree })
	}
	if q.MinWiFi != nil {
		w := *q.MinWiFi
		ps = append(ps, func(c Candidate) bool { return c.WiFi <= w })
	}
	if q.NeedWiFi {
		ps = append(ps, func(c Candidate) bool { return c.WiFi.Reliable() })
	}
	if q.For > 0 {
		ps = append(ps, func(c Candidate) bool {
			return c.FreeUntil.IsZero() || c.FreeUntil.Sub(q.Now) >= q.For
		})
	}
	return fn.And(ps...)
}

// seatsFree uses the published count. A suppressed count means "under 10",
// so assume the worst case of 9 people.
func seatsFree(c Candidate) int {
	used := int(c.Estimate.Count.Value)
	if c.Estimate.Count.Suppressed {
		used = 9
	}
	return max(0, int(c.Room.Capacity)-used)
}

// Score ranks a candidate from 0 to 100. Pure function.
func Score(c Candidate, q Query) float64 {
	free := 1 - math.Min(c.Estimate.Ratio, 1)
	wifi := map[campus.WiFiQuality]float64{campus.WiFiExcellent: 1, campus.WiFiGood: 0.8, campus.WiFiFair: 0.5, campus.WiFiPoor: 0.15, campus.WiFiNone: 0}[c.WiFi]
	later := 1 - math.Min(c.Forecast60.Ratio, 1)
	quiet := 1.0
	if !c.FreeUntil.IsZero() {
		quiet = math.Min(c.FreeUntil.Sub(q.Now).Hours()/3, 1)
	}
	typeBonus := map[campus.RoomType]float64{campus.RoomStudyRoom: 1, campus.RoomLibraryArea: 0.9, campus.RoomClassroom: 0.6, campus.RoomMeeting: 0.5}[c.Room.Type]
	return math.Round((35*free+25*wifi+15*later+15*quiet+10*typeBonus)*10) / 10
}

// Recommend filters, scores and explains. Candidates are not modified.
func Recommend(cands []Candidate, q Query) []Recommendation {
	if q.MaxLevel == 0 {
		q.MaxLevel = campus.OccupancyHigh
	}
	keep := Filters(q)
	out := fn.Map(fn.Filter(cands, keep), func(c Candidate) Recommendation { return explain(c, q) })
	slices.SortStableFunc(out, func(a, b Recommendation) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	if q.Limit > 0 {
		out = fn.Take(out, q.Limit)
	}
	return out
}

func explain(c Candidate, q Query) Recommendation {
	r := Recommendation{RoomID: c.Room.ID, Room: c.Room.Name, Building: c.Building, BuildingID: c.Room.BuildingID,
		Level: c.Room.Level, Type: c.Room.Type, Estimate: c.Estimate, WiFi: c.WiFi, Forecast60: c.Forecast60.Level,
		Score: Score(c, q)}
	seats := seatsFree(c)
	if c.Estimate.Count.Suppressed {
		r.Available = "most seats free"
	} else {
		r.Available = fmt.Sprintf("about %d seats free", seats)
	}
	switch c.Estimate.Level {
	case campus.OccupancyLow:
		r.Reasons = append(r.Reasons, "Quiet right now")
	case campus.OccupancyModerate:
		r.Reasons = append(r.Reasons, "Moderately busy right now")
	}
	if !c.FreeUntil.IsZero() {
		t := c.FreeUntil
		r.QuietUntil = &t
		r.Reasons = append(r.Reasons, "No class until "+t.Format("3:04 PM"))
	} else {
		r.Reasons = append(r.Reasons, "No more classes today")
	}
	if c.Forecast60.Level <= campus.OccupancyModerate {
		r.Reasons = append(r.Reasons, "Expected to stay "+c.Forecast60.Level.String()+" for the next hour")
	} else {
		r.Reasons = append(r.Reasons, "Likely to get busier within the hour")
	}
	switch c.WiFi {
	case campus.WiFiExcellent, campus.WiFiGood:
		r.Reasons = append(r.Reasons, "Strong Wi-Fi")
	case campus.WiFiFair:
		r.Warning = "Wi-Fi here is congested or patchy right now"
	case campus.WiFiPoor:
		r.Warning = "Weak Wi-Fi: this space is in a known dead zone"
	case campus.WiFiNone:
		r.Warning = "No Wi-Fi: the access point for this space is down"
	}
	return r
}
