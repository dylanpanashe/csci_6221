// Package events provides a small generic publish/subscribe bus. The engine
// publishes updates on it; the WebSocket hub, the history collector and
// tests subscribe.
package events

import (
	"sync"
	"sync/atomic"
)

// Bus fans every published value out to all current subscribers.
//
// Publish never blocks. Each subscriber has its own buffered channel; if a
// subscriber falls behind and its buffer is full, the value is dropped for
// that subscriber only and counted. A slow browser tab therefore cannot
// stall the pipeline.
type Bus[T any] struct {
	mu      sync.RWMutex
	subs    map[int]chan T
	nextID  int
	closed  bool
	dropped atomic.Int64
}

// NewBus creates an empty bus.
func NewBus[T any]() *Bus[T] {
	return &Bus[T]{subs: map[int]chan T{}}
}

// Subscribe returns a receive-only channel and a function that removes the
// subscription. The cancel function is safe to call more than once.
func (b *Bus[T]) Subscribe(buffer int) (<-chan T, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan T, max(buffer, 1))
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
}

// Publish delivers v to every subscriber that has room for it. It holds
// the read lock while sending, so Close and unsubscribe (which take the
// write lock) cannot close a channel mid-send.
func (b *Bus[T]) Publish(v T) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, ch := range b.subs {
		select {
		case ch <- v:
		default:
			b.dropped.Add(1)
		}
	}
}

// Close closes every subscriber channel. Later publishes are ignored.
func (b *Bus[T]) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		close(ch)
		delete(b.subs, id)
	}
}

// Subscribers returns the current subscriber count.
func (b *Bus[T]) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Dropped returns how many deliveries were skipped for slow subscribers.
func (b *Bus[T]) Dropped() int64 { return b.dropped.Load() }
