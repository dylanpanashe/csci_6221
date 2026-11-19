package store

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

func TestRing(t *testing.T) {
	r := NewRing[int](3)
	if _, ok := r.Last(); ok {
		t.Fatal("empty ring has a last value")
	}
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	if got := r.Slice(); !slices.Equal(got, []int{3, 4, 5}) || r.Len() != 3 {
		t.Fatalf("slice = %v", got)
	}
	if v, _ := r.Last(); v != 5 {
		t.Fatalf("last = %d", v)
	}
}

func TestAlertsDedupeAndResolve(t *testing.T) {
	s := New(10)
	a := campus.Alert{ID: "A1", AccessPoint: "x", Kind: campus.DiagnosisCongestion}
	if _, ok := s.RaiseAlert(a); !ok {
		t.Fatal("first raise refused")
	}
	if _, ok := s.RaiseAlert(a); ok {
		t.Fatal("duplicate accepted")
	}
	if n := s.ResolveAlerts("x", campus.DiagnosisCongestion, time.Now()); n != 1 {
		t.Fatalf("resolved %d", n)
	}
	if len(s.Alerts(true)) != 0 || len(s.Alerts(false)) != 1 {
		t.Fatal("active/all lists wrong")
	}
}

func TestReadsReturnCopies(t *testing.T) {
	s := New(10)
	s.PutPeople(time.Now(), map[campus.RoomID]float64{"r": 5})
	p, _ := s.People()
	p["r"] = 99
	again, _ := s.People()
	if again["r"] != 5 {
		t.Fatal("caller mutated store state through a returned map")
	}
	if _, err := s.RoomEstimate("missing"); !errors.Is(err, campus.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// Many readers and writers at once; run with -race.
func TestConcurrentAccess(t *testing.T) {
	s := New(50)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				now := time.Now()
				s.PutEstimates([]campus.OccupancyEstimate{{RoomID: "r", At: now}})
				s.Record(now, map[string]float64{"x": float64(i)})
				s.RaiseAlert(campus.Alert{AccessPoint: campus.AccessPointID(rune('a' + w)), Kind: campus.DiagnosisCongestion})
				s.ResolveAlerts(campus.AccessPointID(rune('a'+w)), campus.DiagnosisCongestion, now)
			}
		}(w)
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				_ = s.RoomEstimates()
				_ = s.Series("x")
				_ = s.Alerts(false)
			}
		}()
	}
	wg.Wait()
}
