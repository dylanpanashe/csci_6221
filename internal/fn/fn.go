// Package fn holds small generic helpers for working with slices and
// functions in a functional style: Map, Filter, Reduce, GroupBy, predicate
// combinators and function composition.
//
// Every function here is pure. None of them mutate their inputs; they return
// new slices or new functions. That makes them safe to call from any
// goroutine without locks.
package fn

import (
	"cmp"
	"slices"
)

// Number is a type constraint for anything that can be summed.
type Number interface {
	~int | ~int32 | ~int64 | ~float32 | ~float64
}

// Map applies f to every element and returns a new slice.
func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

// Filter returns the elements for which keep returns true.
func Filter[T any](xs []T, keep func(T) bool) []T {
	out := make([]T, 0, len(xs))
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

// Reduce folds the slice into a single value.
func Reduce[T, A any](xs []T, init A, f func(A, T) A) A {
	acc := init
	for _, x := range xs {
		acc = f(acc, x)
	}
	return acc
}

// SumBy adds up a numeric projection of each element.
func SumBy[T any, N Number](xs []T, f func(T) N) N {
	var total N
	for _, x := range xs {
		total += f(x)
	}
	return total
}

// GroupBy buckets elements by key.
func GroupBy[T any, K comparable](xs []T, key func(T) K) map[K][]T {
	out := make(map[K][]T)
	for _, x := range xs {
		k := key(x)
		out[k] = append(out[k], x)
	}
	return out
}

// SortedBy returns a sorted copy; the input slice is left untouched.
func SortedBy[T any, K cmp.Ordered](xs []T, key func(T) K) []T {
	out := slices.Clone(xs)
	slices.SortStableFunc(out, func(a, b T) int { return cmp.Compare(key(a), key(b)) })
	return out
}

// Take returns at most n elements from the front.
func Take[T any](xs []T, n int) []T {
	if n >= len(xs) {
		return xs
	}
	return xs[:n]
}

// ---------------------------------------------------------------------------
// Predicates
// ---------------------------------------------------------------------------

// Predicate is a reusable yes/no test.
type Predicate[T any] func(T) bool

// And returns a predicate that passes only when every predicate passes.
func And[T any](ps ...Predicate[T]) Predicate[T] {
	return func(x T) bool {
		for _, p := range ps {
			if !p(x) {
				return false
			}
		}
		return true
	}
}

// Or returns a predicate that passes when any predicate passes.
func Or[T any](ps ...Predicate[T]) Predicate[T] {
	return func(x T) bool {
		for _, p := range ps {
			if p(x) {
				return true
			}
		}
		return false
	}
}

// Not inverts a predicate.
func Not[T any](p Predicate[T]) Predicate[T] {
	return func(x T) bool { return !p(x) }
}

// ---------------------------------------------------------------------------
// Composition
// ---------------------------------------------------------------------------

// Step is one fallible transformation in a processing chain.
type Step[T any] func(T) (T, error)

// Chain composes steps left to right into a single step. The first error
// stops the chain, which is how the observation pipeline runs
// validate → normalise → privacy-hash as one function.
func Chain[T any](steps ...Step[T]) Step[T] {
	return func(x T) (T, error) {
		var err error
		for _, s := range steps {
			if x, err = s(x); err != nil {
				return x, err
			}
		}
		return x, nil
	}
}

// Compose joins two plain functions: Compose(f, g)(x) == g(f(x)).
func Compose[A, B, C any](f func(A) B, g func(B) C) func(A) C {
	return func(a A) C { return g(f(a)) }
}

// Clamp keeps v within [lo, hi].
func Clamp[T cmp.Ordered](v, lo, hi T) T {
	return max(lo, min(hi, v))
}
