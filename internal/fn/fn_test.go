package fn

import (
	"errors"
	"slices"
	"strconv"
	"testing"
)

func TestMapFilterReduce(t *testing.T) {
	xs := []int{1, 2, 3, 4, 5}
	sq := Map(xs, func(n int) int { return n * n })
	if !slices.Equal(sq, []int{1, 4, 9, 16, 25}) {
		t.Errorf("Map = %v", sq)
	}
	even := Filter(xs, func(n int) bool { return n%2 == 0 })
	if !slices.Equal(even, []int{2, 4}) {
		t.Errorf("Filter = %v", even)
	}
	sum := Reduce(xs, 0, func(a, n int) int { return a + n })
	if sum != 15 {
		t.Errorf("Reduce = %d", sum)
	}
	if got := SumBy(xs, func(n int) float64 { return float64(n) / 2 }); got != 7.5 {
		t.Errorf("SumBy = %v", got)
	}
	strs := Map(xs, strconv.Itoa)
	if strs[4] != "5" {
		t.Errorf("Map to string = %v", strs)
	}
}

func TestSortedByDoesNotMutate(t *testing.T) {
	in := []int{3, 1, 2}
	out := SortedBy(in, func(n int) int { return n })
	if !slices.Equal(in, []int{3, 1, 2}) || !slices.Equal(out, []int{1, 2, 3}) {
		t.Fatalf("in=%v out=%v", in, out)
	}
}

func TestGroupBy(t *testing.T) {
	g := GroupBy([]string{"apple", "avocado", "banana"}, func(s string) byte { return s[0] })
	if len(g['a']) != 2 || len(g['b']) != 1 {
		t.Fatalf("GroupBy = %v", g)
	}
}

func TestPredicates(t *testing.T) {
	pos := Predicate[int](func(n int) bool { return n > 0 })
	even := Predicate[int](func(n int) bool { return n%2 == 0 })
	tests := []struct {
		name string
		p    Predicate[int]
		in   int
		want bool
	}{
		{"and true", And(pos, even), 4, true},
		{"and false", And(pos, even), 3, false},
		{"or", Or(pos, even), -2, true},
		{"not", Not(pos), -1, true},
	}
	for _, tt := range tests {
		if got := tt.p(tt.in); got != tt.want {
			t.Errorf("%s(%d) = %v", tt.name, tt.in, got)
		}
	}
}

func TestChainStopsAtFirstError(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	inc := Step[int](func(n int) (int, error) { calls++; return n + 1, nil })
	fail := Step[int](func(n int) (int, error) { calls++; return n, boom })
	if got, err := Chain(inc, inc)(1); err != nil || got != 3 {
		t.Fatalf("Chain = %d, %v", got, err)
	}
	calls = 0
	if _, err := Chain(inc, fail, inc)(1); !errors.Is(err, boom) || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestComposeAndClamp(t *testing.T) {
	f := Compose(func(n int) int { return n * 2 }, strconv.Itoa)
	if f(21) != "42" {
		t.Fatal("Compose")
	}
	if Clamp(5, 0, 3) != 3 || Clamp(-1, 0, 3) != 0 || Clamp(2, 0, 3) != 2 {
		t.Fatal("Clamp")
	}
}
