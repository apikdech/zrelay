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

	// hook, when set, runs inside onChange after the change is recorded.
	hook atomic.Pointer[func(BreakerState)]

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
			if h := r.hook.Load(); h != nil {
				(*h)(s)
			}
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

// TestOnChangeMayCallBackIntoBreaker is the lock-order regression: a callback
// that re-enters the breaker (State) while another goroutine reports a
// failure used to deadlock, and a slow callback used to stall every State and
// Wait caller behind the breaker's lock.
func TestOnChangeMayCallBackIntoBreaker(t *testing.T) {
	r := newRig(t, false)
	inCb := make(chan struct{})
	proceed := make(chan struct{})
	reentered := make(chan struct{})
	hook := func(s BreakerState) {
		if s != Closed {
			return
		}
		close(inCb)
		<-proceed
		r.b.State() // must not block
		close(reentered)
	}
	r.hook.Store(&hook)

	r.b.Failure(true)
	r.waitFor(t, r.armed)
	r.clk.Advance(testBase)
	<-inCb // state is already Closed; the Closed callback is running

	// A slow callback must not stall the breaker's readers or writers.
	finished := make(chan struct{})
	go func() {
		r.b.State()
		if err := r.b.Wait(context.Background()); err != nil {
			t.Error(err)
		}
		r.b.Success()
		r.b.Failure(true) // reopens; its callback is queued behind the running one
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("breaker calls blocked behind a running onChange callback")
	}

	close(proceed)
	select {
	case <-reentered:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange calling State() deadlocked")
	}
	r.waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.changes) == 4
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	want := []BreakerState{Open, HalfOpen, Closed, Open}
	for i := range want {
		if r.changes[i] != want[i] {
			t.Fatalf("changes = %v, want %v (in order)", r.changes, want)
		}
	}
}

// TestSinceMarksOutageStart pins the State() semantics: since is when the
// breaker left Closed and survives Open/HalfOpen probe cycling, so a consumer
// (healthz, status/relay.json) can tell how long an outage has lasted.
func TestSinceMarksOutageStart(t *testing.T) {
	r := newRig(t, false)
	r.probeFn = func(n int) error {
		if n < 3 {
			return errors.New("down")
		}
		return nil
	}
	r.b.Failure(true)
	r.waitFor(t, r.armed)
	for i, adv := range []time.Duration{testBase, 2 * testBase} {
		r.clk.Advance(adv)
		r.waitFor(t, func() bool { return int(r.probes.Load()) == i+1 && r.armed() })
		s, since := r.b.State()
		if s != Open || !since.Equal(epoch) {
			t.Fatalf("after failed probe %d: state=%d since=%v, want Open since %v", i+1, s, since, epoch)
		}
	}
	// HalfOpen keeps the outage start too.
	block := make(chan struct{})
	entered := make(chan struct{})
	r.probeFn = func(int) error { close(entered); <-block; return nil }
	r.clk.Advance(4 * testBase)
	<-entered
	if s, since := r.b.State(); s != HalfOpen || !since.Equal(epoch) {
		t.Fatalf("HalfOpen: state=%d since=%v, want since %v", s, since, epoch)
	}
	r.clk.Advance(time.Minute) // time passes during the probe
	closeAt := r.clk.Now()
	close(block)
	if err := r.b.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, since := r.b.State(); s != Closed || !since.Equal(closeAt) {
		t.Fatalf("after recovery: state=%d since=%v, want Closed since %v", s, since, closeAt)
	}
}

// TestProbeIsBoundedByTimeout: a probe that hangs (blackholed endpoint) gets
// its context cancelled after the (unexported) probe timeout, counts as a failed probe and
// reopens the breaker with the delay doubled.
func TestProbeIsBoundedByTimeout(t *testing.T) {
	const probeTimeout = 10 * time.Second
	clk := clock.NewFake(epoch)
	var probes atomic.Int32
	entered := make(chan struct{}, 4)
	b := NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: testBase, MaxDelay: testMax}, clk,
		func(ctx context.Context) error {
			probes.Add(1)
			entered <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}, nil)
	b.probeTimeout = probeTimeout
	var maxes []time.Duration
	var mmu sync.Mutex
	b.jitter = func(max time.Duration) time.Duration {
		mmu.Lock()
		maxes = append(maxes, max)
		mmu.Unlock()
		return max
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	b.Failure(true)
	for clk.Pending() != 1 {
		time.Sleep(time.Millisecond)
	}
	clk.Advance(testBase)
	<-entered
	if s, _ := b.State(); s != HalfOpen {
		t.Fatalf("state during probe = %d, want HalfOpen", s)
	}
	clk.Advance(probeTimeout - time.Nanosecond)
	time.Sleep(20 * time.Millisecond)
	if s, _ := b.State(); s != HalfOpen {
		t.Fatalf("state before probe timeout = %d, want HalfOpen", s)
	}
	clk.Advance(time.Nanosecond)
	deadline := time.Now().Add(10 * time.Second)
	for {
		mmu.Lock()
		n := len(maxes)
		mmu.Unlock()
		if s, _ := b.State(); s == Open && n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hung probe was not cancelled by the probe timeout")
		}
		time.Sleep(time.Millisecond)
	}
	mmu.Lock()
	defer mmu.Unlock()
	if maxes[1] != 2*testBase {
		t.Fatalf("delay bound after timed-out probe = %v, want %v", maxes[1], 2*testBase)
	}
}

// TestProbeTimeoutDefault pins the internal 30 s probe bound; it is not a
// BreakerConfig knob because design §6.3 defines only failure_threshold,
// base_delay and max_delay.
func TestProbeTimeoutDefault(t *testing.T) {
	b := NewBreaker(BreakerConfig{}, clock.NewFake(epoch), nil, nil)
	if b.probeTimeout != 30*time.Second {
		t.Fatalf("probeTimeout = %v, want 30s", b.probeTimeout)
	}
}

// TestNewBreakerDefaults pins the design §6.3 defaults (threshold 5, base 1 s,
// max 5 min) that apply when BreakerConfig is zero, and the MaxDelay clamp.
func TestNewBreakerDefaults(t *testing.T) {
	clk := clock.NewFake(epoch)
	var probes atomic.Int32
	b := NewBreaker(BreakerConfig{}, clk, func(ctx context.Context) error {
		probes.Add(1)
		return errors.New("still down")
	}, nil)
	if b.cfg.FailureThreshold != 5 || b.cfg.BaseDelay != time.Second || b.cfg.MaxDelay != 5*time.Minute {
		t.Fatalf("defaults = %+v, want {5 1s 5m0s}", b.cfg)
	}
	var mmu sync.Mutex
	var maxes []time.Duration
	b.jitter = func(max time.Duration) time.Duration {
		mmu.Lock()
		maxes = append(maxes, max)
		mmu.Unlock()
		return max
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	for range 4 {
		b.Failure(false)
	}
	if s, _ := b.State(); s != Closed {
		t.Fatalf("after 4 transient failures state = %d, want Closed", s)
	}
	b.Failure(false)
	if s, _ := b.State(); s != Open {
		t.Fatalf("after 5 transient failures state = %d, want Open", s)
	}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		deadline := time.Now().Add(10 * time.Second)
		for clk.Pending() != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: breaker never armed its delay timer", i)
			}
			time.Sleep(time.Millisecond)
		}
		mmu.Lock()
		got := maxes[len(maxes)-1]
		mmu.Unlock()
		if got != w {
			t.Fatalf("cycle %d: jitter bound = %v, want %v", i, got, w)
		}
		clk.Advance(w)
		for probes.Load() != int32(i+1) {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: probe never ran", i)
			}
			time.Sleep(time.Millisecond)
		}
		for s, _ := b.State(); s != Open; s, _ = b.State() {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: breaker never reopened", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func TestNewBreakerClampsMaxDelayToBase(t *testing.T) {
	b := NewBreaker(BreakerConfig{BaseDelay: 10 * time.Second, MaxDelay: 2 * time.Second}, clock.NewFake(epoch), nil, nil)
	if b.cfg.MaxDelay != 10*time.Second {
		t.Fatalf("MaxDelay = %v, want clamped up to BaseDelay 10s", b.cfg.MaxDelay)
	}
}

// TestOnChangePanicDoesNotSilenceLaterTransitions: a panicking onChange that
// is recovered by the caller must not leave the notifier flag stuck, or every
// later transition would go undelivered.
func TestOnChangePanicDoesNotSilenceLaterTransitions(t *testing.T) {
	clk := clock.NewFake(epoch)
	var mu sync.Mutex
	var got []BreakerState
	first := true
	b := NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: testBase, MaxDelay: testMax}, clk,
		func(context.Context) error { return nil },
		func(s BreakerState) {
			mu.Lock()
			if first {
				first = false
				mu.Unlock()
				panic("boom")
			}
			got = append(got, s)
			mu.Unlock()
		})
	func() {
		defer func() { _ = recover() }()
		b.Failure(true) // Open; onChange panics
	}()
	// Back to Closed by hand is not possible without Run, so drive a second
	// transition the only other way: a fresh Run cycle.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for clk.Pending() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("breaker never armed its delay timer")
		}
		time.Sleep(time.Millisecond)
	}
	clk.Advance(testBase)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("later transitions not delivered after a recovered panic; got %v", got)
		}
		time.Sleep(time.Millisecond)
	}
}
