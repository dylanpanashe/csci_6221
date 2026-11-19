package store

// Ring is a fixed-capacity circular buffer. Once full, each Push overwrites
// the oldest value, so history uses constant memory however long the
// simulation runs. It is generic over the element type.
//
// Ring is not safe for concurrent use on its own; the Store's lock guards it.
type Ring[T any] struct {
	buf  []T
	next int
	full bool
}

// NewRing creates a ring holding up to size values.
func NewRing[T any](size int) *Ring[T] {
	if size < 1 {
		size = 1
	}
	return &Ring[T]{buf: make([]T, size)}
}

// Push adds a value, overwriting the oldest when full.
func (r *Ring[T]) Push(v T) {
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Len is the number of values held.
func (r *Ring[T]) Len() int {
	if r.full {
		return len(r.buf)
	}
	return r.next
}

// Slice returns the values oldest first, as a new slice.
func (r *Ring[T]) Slice() []T {
	if !r.full {
		return append([]T(nil), r.buf[:r.next]...)
	}
	out := make([]T, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	return append(out, r.buf[:r.next]...)
}

// Last returns the newest value.
func (r *Ring[T]) Last() (T, bool) {
	var zero T
	if r.Len() == 0 {
		return zero, false
	}
	return r.buf[(r.next-1+len(r.buf))%len(r.buf)], true
}
