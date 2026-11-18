package campus

import (
	"errors"
	"fmt"
	"strconv"
)

// Sentinel errors. Callers compare with errors.Is, which also sees through
// any wrapping added by fmt.Errorf("...: %w", err) on the way up.
var (
	ErrNotFound         = errors.New("not found")
	ErrInvalidInput     = errors.New("invalid input")
	ErrPipelineClosed   = errors.New("pipeline closed")
	ErrSimulationPaused = errors.New("simulation paused")
	ErrAPOffline        = errors.New("access point offline")
)

// NotFoundError says which kind of thing was missing. It matches ErrNotFound
// through errors.Is, and callers who want the details use errors.As.
type NotFoundError struct {
	Kind string
	ID   string
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("%s %q not found", e.Kind, e.ID) }

// Is makes errors.Is(err, ErrNotFound) true for any *NotFoundError.
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// ValidationError reports a bad field value.
type ValidationError struct {
	Field  string
	Value  any
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s (%v): %s", e.Field, e.Value, e.Reason)
}

// Is makes errors.Is(err, ErrInvalidInput) true for any *ValidationError.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidInput }

// ValidationErrors collects several problems found in one pass.
type ValidationErrors []*ValidationError

func (v ValidationErrors) Error() string {
	switch len(v) {
	case 0:
		return "no validation errors"
	case 1:
		return v[0].Error()
	default:
		return fmt.Sprintf("%s (and %d more)", v[0].Error(), len(v)-1)
	}
}

// Unwrap exposes every contained error to errors.Is and errors.As
// (multi-error unwrapping, Go 1.20+).
func (v ValidationErrors) Unwrap() []error {
	out := make([]error, len(v))
	for i, e := range v {
		out[i] = e
	}
	return out
}

// OrNil returns nil when there are no errors. Returning a nil slice typed as
// ValidationErrors would give a non-nil error interface, a classic Go trap.
func (v ValidationErrors) OrNil() error {
	if len(v) == 0 {
		return nil
	}
	return v
}

func itoa(n int) string { return strconv.Itoa(n) }
