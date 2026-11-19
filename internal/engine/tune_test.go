package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/accuracy"
	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/forecast"
	"github.com/dylanpanashe/csci_6221/internal/occupancy"
	"github.com/dylanpanashe/csci_6221/internal/pipeline"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
	"github.com/dylanpanashe/csci_6221/internal/simclock"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
)

type rec struct {
	agg   pipeline.AggregatedTick
	truth map[campus.RoomID]float64
	sched *simulator.Schedule
}

// TestTune is an offline tuning harness, skipped unless TUNE=1 is set.
// It records two simulated days (simulator + ingest pipeline), then replays
// the recording through different occupancy and forecast settings and
// prints the error of each against ground truth. Set DAY2=1 to score
// forecasts on the second day only, after the model has learned history.
//
//	TUNE=1 go test -run TestTune -v ./internal/engine/
func TestTune(t *testing.T) {
	if os.Getenv("TUNE") == "" {
		t.Skip()
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	loc, _ := time.LoadLocation("America/New_York")
	c := campus.NewGWUCampus()
	clk, _ := simclock.New(time.Date(2026, 11, 18, 8, 0, 0, 0, loc), 3600, nil)
	sim, _ := simulator.New(simulator.Config{Population: 2600, Seed: 7, Shards: 4, Tick: 33 * time.Millisecond, Location: loc}, c, clk, quiet)
	h, _ := privacy.NewHasher("x", loc)
	p, _ := pipeline.New(pipeline.Config{Workers: 4}, c, h, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	outs := p.Start(ctx, sim)
	go func() {
		for range outs.Telemetry {
		}
	}()
	go func() {
		for range outs.Frames {
		}
	}()
	var recs []rec
	for a := range outs.Aggregates {
		tr := sim.Truth()
		if tr.Seq != a.Seq {
			continue
		}
		m := map[campus.RoomID]float64{}
		for k, v := range tr.Rooms {
			m[k] = float64(v)
		}
		recs = append(recs, rec{a, m, sim.Schedule()})
		if a.At.Day() == 19 && a.At.Hour() >= 21 {
			break
		}
	}
	cancel()
	t.Logf("recorded %d ticks", len(recs))
	fusion := occupancy.NewFusion(occupancy.DefaultWeights, nil)
	fc, _ := forecast.NewEngine(c, nil, "rule_based")
	type variant struct {
		name string
		p    forecast.Params
	}
	var vs []variant
	for _, tau := range []time.Duration{45 * time.Minute, 60 * time.Minute, 90 * time.Minute, 120 * time.Minute, 180 * time.Minute} {
		for _, sc := range []float64{0.5, 1, 1.5} {
			n := fmt.Sprintf("tau=%v surge=%.1f", tau, sc)
			forecast.Strategies[n] = forecast.NewRuleBased(forecast.Params{FreeTau: tau, SurgeScale: sc})
			vs = append(vs, variant{n, forecast.Params{}})
		}
	}
	vs = append(vs, variant{"persistence", forecast.Params{}})
	preds := map[string][]map[campus.RoomID]float64{}
	for _, r := range recs {
		raw := map[campus.RoomID]float64{}
		for _, room := range c.AllRooms() {
			e := fusion.Estimate(campus.EstimateInput{Room: room, Devices: r.agg.Rooms[room.ID], ActiveEvents: r.sched.EventsAt(room.ID, r.agg.At), DevicesPerUser: 1.65, At: r.agg.At})
			raw[room.ID] = float64(e.Count.Value)
		}
		fc.Learn(r.agg.At, raw, r.sched)
		snap := forecast.Snapshot{At: r.agg.At, People: raw, Timetable: r.sched}
		for _, v := range vs {
			preds[v.name] = append(preds[v.name], fc.Predict(snap, time.Hour, v.name))
		}
	}
	byB := func(m map[campus.RoomID]float64) map[campus.RoomID]float64 {
		o := map[campus.RoomID]float64{}
		for id, v := range m {
			r, _ := c.Room(id)
			o[campus.RoomID(r.BuildingID)] += v
		}
		return o
	}
	for _, v := range vs {
		var sb, sr float64
		n := 0
		for i := 0; i+30 < len(recs); i++ {
			if (os.Getenv("DAY2") != "") != (recs[i+30].agg.At.Day() == 19) {
				continue
			}
			tr := recs[i+30].truth
			tot := 0.0
			for _, x := range tr {
				tot += x
			}
			if tot < 100 {
				continue
			}
			wb, _ := accuracy.WAPE(byB(preds[v.name][i]), byB(tr))
			wr, _ := accuracy.WAPE(preds[v.name][i], tr)
			sb += wb
			sr += wr
			n++
		}
		fmt.Printf("FC %-28s building=%.3f room=%.3f\n", v.name, sb/float64(n), sr/float64(n))
	}
	errT := map[string]float64{}
	truT := map[string]float64{}
	for i := 0; i+30 < len(recs); i++ {
		for _, room := range c.AllRooms() {
			d := preds["tau=1h0m0s surge=1.0"][i][room.ID] - recs[i+30].truth[room.ID]
			k := room.Type.String()
			if d < 0 {
				k += "-under"
				d = -d
			} else {
				k += "-over"
			}
			errT[k] += d
		}
		for _, room := range c.AllRooms() {
			truT[room.Type.String()] += recs[i+30].truth[room.ID]
		}
	}
	for k := range maps.Keys(errT) {
		fmt.Printf("FCTYPE %-22s err %.0f\n", k, errT[k])
	}
	bias := map[int][2]float64{}
	for i := 0; i+30 < len(recs); i++ {
		h := recs[i+30].agg.At.Hour()
		var ps, ts float64
		for id, v := range preds["tau=45m0s surge=1.0"][i] {
			ps += v
			ts += recs[i+30].truth[id]
		}
		b := bias[h]
		b[0] += ps
		b[1] += ts
		bias[h] = b
	}
	for h := 8; h < 22; h++ {
		fmt.Printf("HOUR %02d pred/true = %.2f (true %.0f)\n", h, bias[h][0]/bias[h][1], bias[h][1]/30)
	}
}
