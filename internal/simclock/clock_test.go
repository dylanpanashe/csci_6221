package simclock

import (
	"sync"
	"testing"
	"time"
)

// fakeReal is a controllable real-time source.
type fakeReal struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeReal) now() time.Time          { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeReal) advance(d time.Duration) { f.mu.Lock(); f.t = f.t.Add(d); f.mu.Unlock() }

func TestSimClockSpeedPauseResume(t *testing.T) {
	real := &fakeReal{t: time.Unix(0, 0)}
	start := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	c, err := New(start, 60, real.now)
	if err != nil {
		t.Fatal(err)
	}
	real.advance(10 * time.Second)
	if got, want := c.Now(), start.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("after 10s at x60: %v, want %v", got, want)
	}

	c.Pause()
	real.advance(time.Hour)
	if got, want := c.Now(), start.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("paused clock moved: %v", got)
	}

	c.Resume()
	if err := c.SetSpeed(120); err != nil {
		t.Fatal(err)
	}
	real.advance(time.Second)
	if got, want := c.Now(), start.Add(12*time.Minute); !got.Equal(want) {
		t.Fatalf("after speed change: %v, want %v", got, want)
	}

	c.Reset()
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("reset: %v, want %v", got, start)
	}
}

func TestSimClockRejectsBadSpeed(t *testing.T) {
	if _, err := New(time.Now(), 0, nil); err == nil {
		t.Fatal("speed 0 should be rejected")
	}
	c, _ := New(time.Now(), 1, nil)
	if err := c.SetSpeed(-1); err == nil {
		t.Fatal("negative speed should be rejected")
	}
}

// Run with -race: concurrent readers and writers must not race.
func TestSimClockConcurrentAccess(t *testing.T) {
	c, _ := New(time.Now(), 60, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				switch j % 4 {
				case 0:
					_ = c.Now()
				case 1:
					_ = c.SetSpeed(float64(1 + i))
				case 2:
					c.Pause()
				case 3:
					c.Resume()
				}
			}
		}(i)
	}
	wg.Wait()
}
