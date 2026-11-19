package accuracy

import (
	"math"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
	"github.com/dylanpanashe/csci_6221/internal/simulator"
)

func TestWAPE(t *testing.T) {
	tests := []struct {
		name  string
		est   map[campus.RoomID]float64
		truth map[campus.RoomID]float64
		want  float64
		ok    bool
	}{
		{"perfect", map[campus.RoomID]float64{"a": 10, "b": 90}, map[campus.RoomID]float64{"a": 10, "b": 90}, 0, true},
		{"10% off", map[campus.RoomID]float64{"a": 110}, map[campus.RoomID]float64{"a": 100}, 0.1, true},
		{"phantom people count as error", map[campus.RoomID]float64{"a": 100, "ghost": 20}, map[campus.RoomID]float64{"a": 100}, 0.2, true},
		{"empty campus is not scored", map[campus.RoomID]float64{"a": 3}, map[campus.RoomID]float64{"a": 0}, 0, false},
	}
	for _, tt := range tests {
		got, ok := WAPE(tt.est, tt.truth)
		if ok != tt.ok || math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: WAPE = %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestEmptyReportHasNoVerdicts(t *testing.T) {
	r := New(campus.NewGWUCampus()).Report(0)
	for _, m := range r.Metrics {
		if m.Met != nil {
			t.Errorf("%s has a verdict with no data", m.Key)
		}
	}
}

func TestEvaluatorScoresAgainstTruth(t *testing.T) {
	c := campus.NewGWUCampus()
	e := New(c)
	t0 := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	truth := func(seq uint64, at time.Time) *simulator.Truth {
		return &simulator.Truth{Seq: seq, At: at, Rooms: map[campus.RoomID]campus.OccupancyCount{"bell-101": 200, "gelman-1a": 100},
			DeadZones: map[campus.AccessPointID]bool{"bell-2-e": true}, APLoad: map[campus.AccessPointID]float64{"bell-1-h": 0.9},
			Faults: []simulator.FaultRecord{{AccessPoint: "seh-1-l", Start: t0}}}
	}
	for i := 0; i < 25; i++ {
		at := t0.Add(time.Duration(i*2) * time.Minute)
		e.ScoreOccupancy(map[campus.RoomID]float64{"bell-101": 220, "gelman-1a": 100}, truth(uint64(i+1), at))
		e.ScoreDiagnostics([]diagnostics.APStatus{
			{AP: "bell-2-e", Devices: 8, Primary: campus.Diagnosis{Kind: campus.DiagnosisDeadZone}, All: []campus.Diagnosis{{Kind: campus.DiagnosisDeadZone}}},
			{AP: "bell-1-h", Devices: 300, Primary: campus.Diagnosis{Kind: campus.DiagnosisCongestion}},
			{AP: "seh-1-l", All: []campus.Diagnosis{{Kind: campus.DiagnosisAPFailure}}},
		}, truth(uint64(i+1), at), 4, 3*time.Minute)
	}
	got := map[string]Metric{}
	for _, m := range e.Report(1234).Metrics {
		got[m.Key] = m
	}
	if m := got["occupancy_error"]; math.Abs(m.Value-20.0/300) > 1e-9 || m.Met == nil || !*m.Met {
		t.Errorf("occupancy = %+v", m)
	}
	if m := got["dead_zone_detection"]; m.Value != 1 {
		t.Errorf("dead zone = %+v", m)
	}
	if m := got["classification"]; m.Value != 1 {
		t.Errorf("classification = %+v", m)
	}
	if m := got["ap_failure_detection"]; m.Samples != 1 || m.Value != 0 {
		t.Errorf("failure = %+v", m)
	}
}
