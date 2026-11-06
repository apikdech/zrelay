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

// BreakerConfig configures a Breaker. Non-positive fields take the design
// defaults (failure_threshold 5, base_delay 1 s, max_delay 5 min).
type BreakerConfig struct {
	FailureThreshold int
	BaseDelay        time.Duration
	MaxDelay         time.Duration
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

	cbMu sync.Mutex // serialises onChange calls in transition order

	mu       sync.Mutex
	state    BreakerState
	since    time.Time
	failures int           // consecutive transient failures while Closed
	bound    time.Duration // current backoff bound (before jitter)
	changed  chan struct{} // closed and replaced on every transition
}

// NewBreaker returns a Closed breaker. probe is the half-open health check
// (HeadBucket); onChange, which may be nil, is called once per state
// transition, in order, outside the breaker's lock.
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
		since:    clk.Now(),
		bound:    cfg.BaseDelay,
		changed:  make(chan struct{}),
	}
}

// fullJitter returns a uniformly random duration in [0, max].
func fullJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// State returns the current state and when it was entered.
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
	b.transition(Open) // unlocks b.mu
}

// transition moves to s, then calls onChange and releases waiters. It must be
// called with b.mu held and releases it.
func (b *Breaker) transition(s BreakerState) {
	b.state = s
	b.since = b.clk.Now()
	old := b.changed
	b.changed = make(chan struct{})
	b.cbMu.Lock()
	b.mu.Unlock()
	defer b.cbMu.Unlock()
	if b.onChange != nil {
		b.onChange(s)
	}
	// Wake waiters only after onChange, so a Wait that returns has seen the
	// Closed notification.
	close(old)
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
		b.transition(HalfOpen)
		err := b.probe(ctx)
		if ctx.Err() != nil {
			return
		}
		b.mu.Lock()
		if err == nil {
			b.bound = b.cfg.BaseDelay
			b.failures = 0
			b.transition(Closed)
		} else {
			b.bound = min(b.bound*2, b.cfg.MaxDelay)
			b.transition(Open)
		}
	}
}
