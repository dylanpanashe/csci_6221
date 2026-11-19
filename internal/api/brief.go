package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/finder"
	"github.com/dylanpanashe/csci_6221/internal/forecast"
)

// BriefItem is one line of the building brief.
type BriefItem struct {
	Label  string `json:"label"`
	Detail string `json:"detail"`
	Tone   string `json:"tone"` // "busy", "quiet", "warning" or "info"
}

// BuildingBrief is the student-facing summary from slide 15 of the
// proposal: how busy it is now, what is coming, where is quiet, and when
// to arrive.
type BuildingBrief struct {
	Building string      `json:"building"`
	At       time.Time   `json:"at"`
	Headline string      `json:"headline"`
	Items    []BriefItem `json:"items"`
	Arrive   string      `json:"arrive,omitempty"`
}

var levelWords = map[campus.OccupancyLevel]string{
	campus.OccupancyLow: "Quiet", campus.OccupancyModerate: "Moderately busy",
	campus.OccupancyHigh: "Busy", campus.OccupancyVeryHigh: "Very busy",
}

// Brief builds the summary from data the handler has already gathered.
// It does no I/O, so it is a pure function of its inputs.
func Brief(b *campus.Building, floors []FloorView, warnings []forecast.ChangeoverWarning,
	recs []finder.Recommendation, sum BuildingSummary, now time.Time) BuildingBrief {

	br := BuildingBrief{Building: b.Name, At: now}
	head := levelWords[sum.Occupancy.Level] + " now."
	if len(warnings) > 0 {
		w := warnings[0]
		head += fmt.Sprintf(" Very busy at %s.", w.EndsAt.Format("3:04"))
	} else if len(sum.Forecast) > 2 && sum.Forecast[2].Level != sum.Occupancy.Level {
		head += fmt.Sprintf(" %s in an hour.", levelWords[sum.Forecast[2].Level])
	}
	br.Headline = head

	for _, w := range warnings {
		br.Items = append(br.Items,
			BriefItem{Label: w.Room, Tone: "busy",
				Detail: fmt.Sprintf("%s, ~%d students, ends %s", w.Event, w.People, w.EndsAt.Format("3:04"))},
			BriefItem{Label: "Lobby, stairs & lifts", Tone: "busy",
				Detail: fmt.Sprintf("packed %s–%s", w.CrowdedFrom.Format("3:04"), w.CrowdedUntil.Format("3:04"))},
		)
	}
	for _, f := range floors {
		if f.ClassNow || f.Occupancy.Level > campus.OccupancyModerate {
			continue
		}
		quiet := f.QuietUntil == nil || f.QuietUntil.Sub(now) >= time.Hour
		if !quiet {
			continue
		}
		until := "for the rest of the day"
		if f.QuietUntil != nil {
			until = "until " + f.QuietUntil.Format("3:04 PM")
		}
		br.Items = append(br.Items, BriefItem{Label: ordinal(f.Level) + " floor", Tone: "quiet",
			Detail: fmt.Sprintf("quiet %s, %s Wi-Fi", until, wifiWord(f.WiFi.Quality))})
	}
	for _, f := range floors {
		for _, r := range f.Rooms {
			if r.Available && !r.WiFi.Reliable() {
				br.Items = append(br.Items, BriefItem{Label: r.Name, Tone: "warning",
					Detail: fmt.Sprintf("free, but %s Wi-Fi", wifiWord(r.WiFi))})
			}
		}
	}
	if len(br.Items) == 0 && len(recs) > 0 {
		br.Items = append(br.Items, BriefItem{Label: recs[0].Room, Tone: "quiet", Detail: strings.Join(recs[0].Reasons, ", ")})
	}
	if len(warnings) > 0 {
		w := warnings[0]
		br.Arrive = fmt.Sprintf("Best time to arrive: before %s or after %s", w.ArriveBefore.Format("3:04"), w.ArriveAfter.Format("3:04"))
	}
	return br
}

func wifiWord(q campus.WiFiQuality) string {
	switch q {
	case campus.WiFiExcellent:
		return "strong"
	case campus.WiFiGood:
		return "good"
	case campus.WiFiFair:
		return "patchy"
	case campus.WiFiPoor:
		return "weak"
	default:
		return "no"
	}
}

func ordinal(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}
