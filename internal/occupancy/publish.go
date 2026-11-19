package occupancy

import (
	"math"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
)

// Publish converts a raw estimate into the privacy-safe version that may
// leave the backend:
//
//   - the headcount gets noise and is suppressed below the threshold;
//   - a suppressed count also hides its range, its device count and its
//     ratio, because each of those could be turned back into the count;
//   - the status level is kept, because "this study room is in use" is the
//     whole point of the product and is not a count.
//
// The input is passed by value, so the raw estimate held by the caller is
// never modified.
func Publish(e campus.OccupancyEstimate, p *privacy.Publisher) campus.OccupancyEstimate {
	raw := float64(e.Count.Value)
	e.Count = p.Publish(raw)
	if e.Count.Suppressed {
		// Hide everything a small count could be worked back out from:
		// the range, the device count (devices ÷ calibration ≈ people) and
		// the ratio (ratio × capacity ≈ people). Only the level stays.
		e.Low, e.High, e.Ratio = 0, 0, 0
		e.Devices = campus.PublicCount{Suppressed: true, Display: e.Count.Display}
		return e
	}
	e.Devices = privacy.Suppress(e.Devices.Value, p.Threshold)
	// Keep the range consistent with the noisy published value.
	shift := float64(e.Count.Value) - raw
	e.Low = campus.OccupancyCount(math.Max(0, float64(e.Low)+shift))
	e.High = campus.OccupancyCount(math.Max(0, float64(e.High)+shift))
	if e.Capacity > 0 {
		e.Ratio = float64(e.Count.Value) / float64(e.Capacity)
		e.Level = campus.LevelForRatio(e.Ratio)
	}
	return e
}

// Aggregate sums raw estimates for a group of rooms (a floor or building)
// before publication. Aggregating first and suppressing second keeps large
// groups useful even when every room in them is small.
func Aggregate(id string, es []campus.OccupancyEstimate) campus.OccupancyEstimate {
	out := campus.OccupancyEstimate{RoomID: campus.RoomID(id)}
	var confW float64
	for _, e := range es {
		out.Count.Value += e.Count.Value
		out.Low += e.Low
		out.High += e.High
		out.Capacity += e.Capacity
		out.Expected += e.Expected
		out.Devices.Value += e.Devices.Value
		confW += e.Confidence * float64(e.Count.Value+1)
		if e.At.After(out.At) {
			out.At = e.At
		}
	}
	if out.Capacity > 0 {
		out.Ratio = float64(out.Count.Value) / float64(out.Capacity)
	}
	out.Level = campus.LevelForRatio(out.Ratio)
	if n := float64(out.Count.Value) + float64(len(es)); n > 0 {
		out.Confidence = confW / n
	}
	return out
}
