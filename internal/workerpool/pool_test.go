package workerpool

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func feed(n int) <-chan int {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for i := 0; i < n; i++ {
			ch <- i
		}
	}()
	return ch
}

func TestPoolProcessesEverything(t *testing.T) {
	p, err := New("square", 4, func(_ context.Context, n int) (int, error) { return n * n, nil })
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	count := 0
	for r := range p.Run(context.Background(), feed(100)) {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		sum += r.Value
		count++
	}
	if count != 100 || sum != 328350 {
		t.Fatalf("count=%d sum=%d", count, sum)
	}
	if s := p.Stats(); s.Processed != 100 || s.InFlight != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestPoolIsBounded(t *testing.T) {
	var cur, peak atomic.Int64
	p, _ := New("bounded", 3, func(_ context.Context, n int) (int, error) {
		c := cur.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		cur.Add(-1)
		return n, nil
	})
	for range p.Run(context.Background(), feed(60)) {
	}
	if peak.Load() > 3 {
		t.Fatalf("peak concurrency %d exceeds 3 workers", peak.Load())
	}
}

func TestPoolRecoversFromPanic(t *testing.T) {
	p, _ := New("panicky", 2, func(_ context.Context, n int) (int, error) {
		if n == 5 {
			panic("bad input")
		}
		return n, nil
	})
	var ok, panics int
	for r := range p.Run(context.Background(), feed(20)) {
		var pe *PanicError
		switch {
		case errors.As(r.Err, &pe):
			panics++
		case r.Err == nil:
			ok++
		}
	}
	if panics != 1 || ok != 19 {
		t.Fatalf("ok=%d panics=%d; the pool should keep going after a panic", ok, panics)
	}
}

func TestPoolStopsOnCancelWithoutLeaking(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan int) // never closed: only cancellation can stop the workers
	p, _ := New("cancel", 8, func(_ context.Context, n int) (int, error) { return n, nil })
	out := p.Run(ctx, in)
	cancel()
	select {
	case _, open := <-out:
		if open {
			t.Fatal("unexpected result")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("output not closed after cancel")
	}
	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines before=%d after=%d: leak", before, after)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New("x", 0, func(context.Context, int) (int, error) { return 0, nil }); !errors.Is(err, ErrInvalidWorkers) {
		t.Fatalf("err = %v", err)
	}
	if _, err := New[int, int]("x", 1, nil); err == nil {
		t.Fatal("nil task accepted")
	}
}

func TestMerge(t *testing.T) {
	total := 0
	for v := range Merge(context.Background(), feed(10), feed(5), feed(0)) {
		total += v
	}
	if total != 45+10 {
		t.Fatalf("total = %d", total)
	}
}
