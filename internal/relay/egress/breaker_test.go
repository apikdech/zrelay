package egress

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

var epoch = time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC)

const testBase, testMax = time.Second, 5 * time.Minute

type breakerRig struct {
	b      *Breaker
	clk    *clock.Fake
	probes atomic.Int32
	// probeFn decides each probe's result; nil means success. Set before the
	// breaker opens.
	probeFn func(n int) error

	mu      sync.Mutex
	changes []BreakerState
	// jitterMax records the upper bound handed to the jitter function.
	jitterMax []time.Duration
	done      chan struct{}
}

// newRig builds a breaker on a fake clock. The jitter is deterministic (always
// the upper bound) unless realJitter is set, in which case a random value in
// [0, bound] is drawn; the bound is recorded either way.
func newRig(t *testing.T, realJitter bool) *breakerRig {
	t.Helper()
	r := &breakerRig{clk: clock.NewFake(epoch), done: make(chan struct{})}
	r.b = NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: testBase, MaxDelay: testMax}, r.clk,
		func(ctx context.Context) error {
			n := int(r.probes.Add(1))
			if r.probeFn != nil {
				return r.probeFn(n)
			}
			return nil
		},
		func(s BreakerState) {
			r.mu.Lock()
			r.changes = append(r.changes, s)
			r.mu.Unlock()
		})
	inner := r.b.jitter
	r.b.jitter = func(max time.Duration) time.Duration {
		r.mu.Lock()
		r.jitterMax = append(r.jitterMax, max)
		r.mu.Unlock()
		if realJitter {
			return inner(max)
		}
		return max
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.b.Run(ctx); close(r.done) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func (r *breakerRig) trip() {
	for range 5 {
		r.b.Failure(false)
	}
}

func (r *breakerRig) waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *breakerRig) state() BreakerState { s, _ := r.b.State(); return s }

func (r *breakerRig) bounds() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.jitterMax...)
}

// armed reports that the breaker is Open with its next probe timer pending.
func (r *breakerRig) armed() bool { return r.state() == Open && r.clk.Pending() == 1 }

func TestBreakerStateValues(t *testing.T) {
	if Closed != 0 || HalfOpen != 1 || Open != 2 {
		t.Fatalf("Closed/HalfOpen/Open = %d/%d/%d, want 0/1/2", Closed, HalfOpen, Open)
	}
}

func TestBreakerOpensAfterThreshold(t *testing.T) {
	r := newRig(t, false)
	for range 4 {
		r.b.Failure(false)
	}
	if got := r.state(); got != Closed {
		t.Fatalf("after 4 transient failures state = %d, want Closed", got)
	}
	r.b.Failure(false)
	if got := r.state(); got != Open {
		t.Fatalf("after 5 transient failures state = %d, want Open", got)
	}
}

func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	r := newRig(t, false)
	for range 4 {
		r.b.Failure(false)
	}
	r.b.Success()
	for range 4 {
		r.b.Failure(false)
	}
	if got := r.state(); got != Closed {
		t.Fatalf("state = %d, want Closed (failures were not consecutive)", got)
	}
}

func TestPermanentOpensImmediately(t *testing.T) {
	r := newRig(t, false)
	r.b.Failure(true)
	s, since := r.b.State()
	if s != Open {
		t.Fatalf("state = %d, want Open", s)
	}
	if !since.Equal(epoch) {
		t.Fatalf("since = %v, want %v", since, epoch)
	}
}

func TestHalfOpenSingleProbe(t *testing.T) {
	r := newRig(t, false)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	r.probeFn = func(int) error { entered <- struct{}{}; <-release; return nil }

	r.b.Failure(true)
	r.waitFor(t, r.armed)

	waited := make(chan error, 1)
	go func() { waited <- r.b.Wait(context.Background()) }()

	r.clk.Advance(testBase - time.Nanosecond)
	if n := r.probes.Load(); n != 0 {
		t.Fatalf("probe ran %d times before the delay elapsed", n)
	}
	r.clk.Advance(time.Nanosecond)
	<-entered
	r.waitFor(t, func() bool { return r.state() == HalfOpen })

	r.clk.Advance(time.Hour)
	select {
	case err := <-waited:
		t.Fatalf("Wait returned %v during the probe", err)
	case <-time.After(20 * time.Millisecond):
	}
	if n := r.probes.Load(); n != 1 {
		t.Fatalf("probe calls = %d, want exactly 1", n)
	}

	close(release)
	if err := <-waited; err != nil {
		t.Fatalf("Wait = %v after probe success", err)
	}
}

func TestWaitReturnsImmediatelyWhenClosed(t *testing.T) {
	r := newRig(t, false)
	if err := r.b.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	r := newRig(t, false)
	r.b.Failure(true)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.b.Wait(ctx) }()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
}

func TestProbeSuccessCloses(t *testing.T) {
	r := newRig(t, false)
	r.probeFn = func(n int) error {
		if n < 3 {
			return errors.New("still down")
		}
		return nil
	}
	r.b.Failure(true)
	r.waitFor(t, r.armed)
	r.clk.Advance(testBase)
	r.waitFor(t, func() bool { return r.probes.Load() == 1 && r.armed() })
	r.clk.Advance(2 * testBase)
	r.waitFor(t, func() bool { return r.probes.Load() == 2 && r.armed() })
	r.clk.Advance(4 * testBase)

	if err := r.b.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.state(); got != Closed {
		t.Fatalf("state = %d, want Closed", got)
	}

	// Backoff was reset: the next outage starts again from the base delay.
	r.b.Failure(true)
	r.waitFor(t, r.armed)
	b := r.bounds()
	if last := b[len(b)-1]; last != testBase {
		t.Fatalf("delay bound after recovery = %v, want base %v", last, testBase)
	}
}

func TestProbeFailureDoublesDelayCapped(t *testing.T) {
	r := newRig(t, true)
	r.probeFn = func(int) error { return errors.New("down") }
	r.b.Failure(true)

	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second,
		256 * time.Second, testMax, testMax}
	for i, w := range want {
		r.waitFor(t, r.armed)
		// The jittered delay is at most w, so advancing by w must fire it.
		r.clk.Advance(w)
		r.waitFor(t, func() bool { return int(r.probes.Load()) == i+1 && r.armed() })
	}
	got := r.bounds()
	if len(got) < len(want) {
		t.Fatalf("recorded %d bounds, want at least %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("delay bound[%d] = %v, want %v", i, got[i], w)
		}
	}
}

func TestJitterWithinBound(t *testing.T) {
	for range 1000 {
		for _, max := range []time.Duration{time.Second, 5 * time.Minute} {
			if d := fullJitter(max); d < 0 || d > max {
				t.Fatalf("fullJitter(%v) = %v, outside [0, max]", max, d)
			}
		}
	}
}

func TestOnChangeCalledPerTransition(t *testing.T) {
	r := newRig(t, false)
	r.probeFn = func(n int) error {
		if n == 1 {
			return errors.New("down")
		}
		return nil
	}
	r.trip()
	r.trip() // already open: no further transition
	r.waitFor(t, r.armed)
	r.clk.Advance(testBase)
	r.waitFor(t, func() bool { return r.probes.Load() == 1 && r.armed() })
	r.clk.Advance(2 * testBase)
	if err := r.b.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	want := []BreakerState{Open, HalfOpen, Open, HalfOpen, Closed}
	if len(r.changes) != len(want) {
		t.Fatalf("changes = %v, want %v", r.changes, want)
	}
	for i := range want {
		if r.changes[i] != want[i] {
			t.Fatalf("changes = %v, want %v", r.changes, want)
		}
	}
}
