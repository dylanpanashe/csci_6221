// Package workerpool provides a generic, bounded worker pool and fan-in
// helper. The pool runs a fixed number of goroutines no matter how much
// work arrives, so a burst of Wi-Fi observations cannot spawn an unbounded
// number of goroutines.
package workerpool

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// TaskFunc processes one input. It receives the pool's context so long tasks
// can stop early when the pool is shut down.
type TaskFunc[In, Out any] func(ctx context.Context, in In) (Out, error)

// Result carries either a value or the error that stopped it.
type Result[Out any] struct {
	Value Out
	Err   error
}

// PanicError is returned in a Result when a task panicked. The worker
// recovers, records the stack, and keeps processing the next input: one bad
// observation must not take down the pipeline.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("task panicked: %v", e.Value) }

// ErrInvalidWorkers is returned by New for a non-positive worker count.
var ErrInvalidWorkers = errors.New("workerpool: workers must be at least 1")

// Stats are updated atomically by workers and can be read at any time.
type Stats struct {
	Processed atomic.Int64
	Failed    atomic.Int64
	Panicked  atomic.Int64
	InFlight  atomic.Int64
}

// Snapshot is a plain copy of Stats for logging and JSON.
type Snapshot struct {
	Workers   int   `json:"workers"`
	Processed int64 `json:"processed"`
	Failed    int64 `json:"failed"`
	Panicked  int64 `json:"panicked"`
	InFlight  int64 `json:"in_flight"`
}

// Pool is a fixed-size group of workers applying one TaskFunc.
// In and Out are type parameters, so the same pool type processes
// observation batches, forecast requests or anything else with full type
// safety and no interface{} casts.
type Pool[In, Out any] struct {
	name    string
	workers int
	task    TaskFunc[In, Out]
	stats   Stats
}

// New creates a pool. It does not start any goroutines until Run.
func New[In, Out any](name string, workers int, task TaskFunc[In, Out]) (*Pool[In, Out], error) {
	if workers < 1 {
		return nil, fmt.Errorf("%s: %w", name, ErrInvalidWorkers)
	}
	if task == nil {
		return nil, fmt.Errorf("%s: nil task", name)
	}
	return &Pool[In, Out]{name: name, workers: workers, task: task}, nil
}

// Run starts the workers. They read from in until it is closed or ctx is
// cancelled. The returned channel is closed after every worker has exited,
// so ranging over it is always safe and never leaks.
//
// Backpressure: out is unbuffered beyond the worker count, so if the
// consumer is slow, workers block on send, stop reading in, and the
// producer feeding in slows down too.
func (p *Pool[In, Out]) Run(ctx context.Context, in <-chan In) <-chan Result[Out] {
	out := make(chan Result[Out], p.workers)
	var wg sync.WaitGroup
	wg.Add(p.workers)
	for i := 0; i < p.workers; i++ {
		go func() {
			defer wg.Done()
			p.work(ctx, in, out)
		}()
	}
	// The closer goroutine is the only one that closes out, and it does so
	// only after all senders are done. That rules out send-on-closed panics.
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func (p *Pool[In, Out]) work(ctx context.Context, in <-chan In, out chan<- Result[Out]) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-in:
			if !ok {
				return
			}
			res := p.safeCall(ctx, item)
			select {
			case out <- res:
			case <-ctx.Done():
				return
			}
		}
	}
}

// safeCall runs the task with panic protection. The deferred recover only
// catches panics from this one call, so the worker loop carries on.
func (p *Pool[In, Out]) safeCall(ctx context.Context, item In) (res Result[Out]) {
	p.stats.InFlight.Add(1)
	defer p.stats.InFlight.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			p.stats.Panicked.Add(1)
			res = Result[Out]{Err: &PanicError{Value: r, Stack: debug.Stack()}}
		}
	}()
	v, err := p.task(ctx, item)
	if err != nil {
		p.stats.Failed.Add(1)
	} else {
		p.stats.Processed.Add(1)
	}
	return Result[Out]{Value: v, Err: err}
}

// Stats returns a consistent-enough copy of the counters.
func (p *Pool[In, Out]) Stats() Snapshot {
	return Snapshot{
		Workers:   p.workers,
		Processed: p.stats.Processed.Load(),
		Failed:    p.stats.Failed.Load(),
		Panicked:  p.stats.Panicked.Load(),
		InFlight:  p.stats.InFlight.Load(),
	}
}

// Name returns the pool's label (used in logs).
func (p *Pool[In, Out]) Name() string { return p.name }

// Merge fans several channels into one (fan-in). The output closes once
// every input has closed or ctx is cancelled.
func Merge[T any](ctx context.Context, inputs ...<-chan T) <-chan T {
	out := make(chan T)
	var wg sync.WaitGroup
	wg.Add(len(inputs))
	for _, in := range inputs {
		go func(in <-chan T) {
			defer wg.Done()
			for v := range in {
				select {
				case out <- v:
				case <-ctx.Done():
					return
				}
			}
		}(in)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
