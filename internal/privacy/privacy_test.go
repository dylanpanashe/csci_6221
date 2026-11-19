package privacy

import (
	mrand "math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

func TestSuppressSmallGroups(t *testing.T) {
	tests := []struct {
		n          campus.OccupancyCount
		suppressed bool
		display    string
	}{
		{0, true, "<10"}, {1, true, "<10"}, {9, true, "<10"}, {10, false, "10"}, {250, false, "250"},
	}
	for _, tt := range tests {
		got := Suppress(tt.n, 10)
		if got.Suppressed != tt.suppressed || got.Display != tt.display {
			t.Errorf("Suppress(%d) = %+v", tt.n, got)
		}
		if got.Suppressed && got.Value != 0 {
			t.Errorf("suppressed count %d leaked its value %d", tt.n, got.Value)
		}
	}
}

// Whatever noise is drawn, nothing under the threshold is ever published.
func TestPublisherNeverExposesSmallCounts(t *testing.T) {
	p := NewPublisher(10, 2.0, 1)
	for i := 0; i < 10000; i++ {
		pc := p.Publish(float64(i % 30))
		if !pc.Suppressed && pc.Value < 10 {
			t.Fatalf("published %d", pc.Value)
		}
	}
}

func TestAddNoiseNeverNegative(t *testing.T) {
	rng := mrand.New(mrand.NewPCG(1, 2))
	for i := 0; i < 1000; i++ {
		if AddNoise(0.5, 5, rng) < 0 {
			t.Fatal("negative count")
		}
	}
	if AddNoise(7.4, 0, nil) != 7 {
		t.Fatal("zero noise should just round")
	}
}

func TestHasherRotatesDaily(t *testing.T) {
	h, err := NewHasher("test-secret", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	a := h.Hash("02:00:00:00:01:aa", day1)
	b := h.Hash("02:00:00:00:01:aa", day1.Add(10*time.Hour))
	c := h.Hash("02:00:00:00:01:aa", day1.Add(24*time.Hour))
	d := h.Hash("02:00:00:00:02:bb", day1)
	if a != b {
		t.Error("same device, same day must hash the same (needed for de-duplication)")
	}
	if a == c {
		t.Error("hash must change the next day")
	}
	if a == d {
		t.Error("different devices collided")
	}
	if a.IsZero() {
		t.Error("zero hash")
	}
}

func TestHasherConcurrent(t *testing.T) {
	h, _ := NewHasher("", time.UTC)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				_ = h.Hash("dev", start.Add(time.Duration(i%3)*24*time.Hour))
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkHash(b *testing.B) {
	h, _ := NewHasher("bench", time.UTC)
	at := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = h.Hash("02:00:12:34:56:78", at)
	}
}
