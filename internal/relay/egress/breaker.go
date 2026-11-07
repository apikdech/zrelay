// Package egress holds the storage-facing resilience primitives of the relay:
// the circuit breaker shared by every upload worker, and per-request retry
// (design §6.3).
package egress

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

// BreakerState is the circuit breaker state. The numeric values are exported
// as the zrelay_storage_breaker_state gauge.
type BreakerState int

const (
	// Closed lets requests flow.
	Closed BreakerState = 0
	// HalfOpen is the single health probe; workers stay parked.
	HalfOpen BreakerState = 1
	// Open parks workers until the backoff delay elapses.
	Open BreakerState = 2
)

// probeTimeout bounds each half-open probe. It is deliberately not a
// BreakerConfig field: design §6.3 defines only failure_threshold, base_delay
// and max_delay.
const probeTimeout = 30 * time.Second

// BreakerConfig configures a Breaker. Non-positive fields take the design
// defaults (failure_threshold 5, base_delay 1 s, max_delay 5 min), and a
// MaxDelay below BaseDelay is raised to BaseDelay.
type BreakerConfig struct {
	FailureThreshold int
	BaseDelay        time.Duration
	MaxDelay         time.Duration
	// OnPermanent, when not nil, is called with the cause each time a
	// permanent failure (see Breaker.FailureErr) opens the breaker, so the
	// operator can tell a rejected credential from an outage (design §6.3).
	// It runs on the failing goroutine, without a breaker lock held, and must
	// not block.
	OnPermanent func(err error)
}

// Breaker is the circuit breaker for one storage endpoint. It is safe for
// concurrent use. Run must be started for an open breaker to recover.
type Breaker struct {
	cfg      BreakerConfig
	clk      clock.Clock
	probe    func(ctx context.Context) error
	onChange func(BreakerState)

	// jitter returns a duration in [0, max]; replaced by tests.
	jitter func(max time.Duration) time.Duration
	// probeTimeout bounds each probe; tests may override it.
	probeTimeout time.Duration

	mu       sync.Mutex
	state    BreakerState
	since    time.Time
	pending  []BreakerState // transitions whose onChange has not been called yet
	notifier bool           // a goroutine is draining pending
	failures int            // consecutive transient failures while Closed
	bound    time.Duration  // current backoff bound (before jitter)
	changed  chan struct{}  // closed and replaced on every transition
}

// NewBreaker returns a Closed breaker. probe is the half-open health check
// (HeadBucket); it is run with a context cancelled after an internal 30 s
// timeout, so a hung endpoint cannot hold the breaker half-open forever, and
// it should return promptly once that context is done. onChange, which may be nil, is
// called once per state transition, in transition order and never
// concurrently with itself, without holding any breaker lock: it may call back
// into the breaker (State, Failure, ...) and may be slow without stalling
// other callers. It runs on the goroutine that caused the transition (or on
// one already delivering earlier transitions), so it is asynchronous with
// respect to State and Wait: those may already show a newer state.
func NewBreaker(cfg BreakerConfig, clk clock.Clock, probe func(ctx context.Context) error, onChange func(BreakerState)) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 5 * time.Minute
	}
	if cfg.MaxDelay < cfg.BaseDelay {
		cfg.MaxDelay = cfg.BaseDelay
	}
	return &Breaker{
		cfg:      cfg,
		clk:      clk,
		probe:    probe,
		onChange: onChange,
		jitter:   fullJitter,

		probeTimeout: probeTimeout,
		since:        clk.Now(),
		bound:        cfg.BaseDelay,
		changed:      make(chan struct{}),
	}
}

// fullJitter returns a uniformly random duration in [0, max].
func fullJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// State returns the current state and a timestamp with these semantics: while
// Closed, when the breaker closed (or was created); while Open or HalfOpen,
// when it last left Closed, i.e. the start of the outage. The timestamp is NOT
// reset by Open/HalfOpen probe cycling, so now minus since is the outage
// length that /healthz ("open longer than 5 minutes") and status/relay.json
// (storage.since) need.
func (b *Breaker) State() (BreakerState, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.since
}

// Wait blocks until the breaker is Closed or ctx is done.
func (b *Breaker) Wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		if b.state == Closed {
			b.mu.Unlock()
			return nil
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// Success records a successful request. While Closed it resets the
// consecutive-failure count; otherwise it is ignored, because only the
// half-open probe may close the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Closed {
		b.failures = 0
	}
}

// Failure records a failed request. A permanent failure, or the
// FailureThreshold-th consecutive transient one, opens the breaker. Failures
// reported while it is already open (late results of in-flight requests) are
// ignored.
func (b *Breaker) Failure(permanent bool) {
	b.FailureErr(nil, permanent)
}

// FailureErr is Failure that also tells the breaker's OnPermanent callback
// the error behind a permanent failure that opens it.
func (b *Breaker) FailureErr(err error, permanent bool) {
	b.mu.Lock()
	if b.state != Closed {
		b.mu.Unlock()
		return
	}
	b.failures++
	if !permanent && b.failures < b.cfg.FailureThreshold {
		b.mu.Unlock()
		return
	}
	b.failures = 0
	b.bound = b.cfg.BaseDelay
	b.transitionLocked(Open)
	b.mu.Unlock()
	if permanent && b.cfg.OnPermanent != nil {
		b.cfg.OnPermanent(err)
	}
	b.notify()
}

// transitionLocked moves to s, queues the onChange notification and wakes
// waiters. It must be called with b.mu held and never blocks; the caller must
// call notify after unlocking. Waiters are woken here, at the state change,
// not after onChange: a Wait that returns has seen the new state, but its
// callback may not have been delivered yet.
func (b *Breaker) transitionLocked(s BreakerState) {
	if s == Closed || b.state == Closed {
		b.since = b.clk.Now() // entering or leaving Closed; Open<->HalfOpen keeps it
	}
	b.state = s
	if b.onChange != nil {
		b.pending = append(b.pending, s)
	}
	close(b.changed)
	b.changed = make(chan struct{})
}

// notify delivers queued onChange notifications in order. At most one goroutine
// drains the queue at a time; one that finds another draining returns at once,
// and the drainer delivers its transition. b.mu is never held, and no other
// lock is taken, while onChange runs. If onChange panics, the panic propagates
// to the caller (which may recover it) but the drainer role is released, so
// later transitions are still delivered; transitions still queued behind the
// panicking one are delivered with the next transition.
func (b *Breaker) notify() {
	b.mu.Lock()
	if b.notifier {
		b.mu.Unlock()
		return
	}
	b.notifier = true
	clean := false
	defer func() {
		if clean {
			return
		}
		b.mu.Lock()
		b.notifier = false
		b.mu.Unlock()
	}()
	for len(b.pending) > 0 {
		s := b.pending[0]
		b.pending = b.pending[1:]
		b.mu.Unlock()
		b.onChange(s)
		b.mu.Lock()
	}
	b.notifier = false
	clean = true
	b.mu.Unlock()
}

// runProbe runs one probe under a context cancelled after b.probeTimeout.
func (b *Breaker) runProbe(ctx context.Context) error {
	pctx, cancel := context.WithCancel(ctx)
	timer := b.clk.NewTimer(b.probeTimeout)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
			cancel()
		case <-stopped:
		}
	}()
	defer func() {
		timer.Stop()
		close(stopped)
		cancel()
	}()
	return b.probe(pctx)
}

// Run drives recovery: whenever the breaker is Open it waits the jittered
// backoff delay, moves to HalfOpen, runs exactly one probe, and then closes
// (resetting the delay) or reopens with the delay doubled up to MaxDelay. It
// returns when ctx is done.
func (b *Breaker) Run(ctx context.Context) {
	for {
		b.mu.Lock()
		if b.state != Open {
			ch := b.changed
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ch:
			}
			continue
		}
		delay := b.jitter(b.bound)
		b.mu.Unlock()

		if err := b.clk.Sleep(ctx, delay); err != nil {
			return
		}

		b.mu.Lock()
		b.transitionLocked(HalfOpen)
		b.mu.Unlock()
		b.notify()

		err := b.runProbe(ctx)
		if ctx.Err() != nil {
			return
		}

		b.mu.Lock()
		if err == nil {
			b.bound = b.cfg.BaseDelay
			b.failures = 0
			b.transitionLocked(Closed)
		} else {
			b.bound = min(b.bound*2, b.cfg.MaxDelay)
			b.transitionLocked(Open)
		}
		b.mu.Unlock()
		b.notify()
	}
}
