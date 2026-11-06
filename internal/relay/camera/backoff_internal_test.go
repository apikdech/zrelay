package camera

import (
	"math/rand/v2"
	"testing"
	"time"
)

var testBackoff = BackoffConfig{Min: time.Second, Max: 60 * time.Second, ResetAfter: 5 * time.Minute}

// TestBackoffFullJitterBounded checks that every delay lies in
// [0, min(Max, Min*2^n)] and that a session stable for ResetAfter restarts
// the sequence (design §4.2).
func TestBackoffFullJitterBounded(t *testing.T) {
	t.Run("random delays stay within the cap", func(t *testing.T) {
		b := newBackoff(testBackoff, rand.Int64N)
		for n := range 40 {
			limit := min(60*time.Second, time.Second<<min(n, 10))
			for range 200 {
				draw := *b // same n, fresh draw
				if d := draw.next(0); d < 0 || d > limit {
					t.Fatalf("failure %d: delay %v outside [0, %v]", n, d, limit)
				}
			}
			b.next(0)
		}
	})

	t.Run("the cap doubles from Min to Max", func(t *testing.T) {
		b := newBackoff(testBackoff, func(n int64) int64 { return n - 1 }) // always the largest draw
		var got []time.Duration
		for range 9 {
			got = append(got, b.next(0))
		}
		want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
		for i, w := range want {
			if got[i] != w*time.Second {
				t.Fatalf("delays = %v, want caps %v seconds", got, want)
			}
		}
	})

	t.Run("a stable session resets the sequence", func(t *testing.T) {
		b := newBackoff(testBackoff, func(n int64) int64 { return n - 1 })
		for range 6 {
			b.next(0)
		}
		if d := b.next(5*time.Minute - time.Nanosecond); d != 60*time.Second {
			t.Errorf("after a 4m59s session delay = %v, want 60s (no reset)", d)
		}
		if d := b.next(5 * time.Minute); d != time.Second {
			t.Errorf("after a 5m session delay = %v, want 1s (reset)", d)
		}
		if d := b.next(0); d != 2*time.Second {
			t.Errorf("next delay = %v, want 2s", d)
		}
	})

	t.Run("zero config uses 1s, 60s, 5m", func(t *testing.T) {
		b := newBackoff(BackoffConfig{}, func(n int64) int64 { return n - 1 })
		if d := b.next(0); d != time.Second {
			t.Errorf("first delay = %v, want 1s", d)
		}
		for range 10 {
			b.next(0)
		}
		if d := b.next(0); d != 60*time.Second {
			t.Errorf("late delay = %v, want 60s", d)
		}
		if d := b.next(5 * time.Minute); d != time.Second {
			t.Errorf("after a 5m session delay = %v, want 1s", d)
		}
	})
}
