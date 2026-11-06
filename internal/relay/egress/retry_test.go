package egress

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

var (
	errTransient = errors.New("transient")
	errPermanent = errors.New("permanent")
	errMissing   = errors.New("missing")
)

func classify(err error) storage.ErrorClass {
	switch {
	case errors.Is(err, errPermanent):
		return storage.Permanent
	case errors.Is(err, errMissing):
		return storage.NotFound
	default:
		return storage.Transient
	}
}

// runDo runs Do in a goroutine and advances the fake clock until it returns,
// reporting how far fake time moved.
func runDo(t *testing.T, clk *clock.Fake, b *Breaker, op func(context.Context) error) (time.Duration, error) {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- Do(context.Background(), clk, classify, b, op) }()
	start := clk.Now()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-res:
			return clk.Now().Sub(start), err
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Do did not return")
		}
		clk.Advance(50 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

func idleBreaker(clk clock.Clock) *Breaker {
	return NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Minute}, clk,
		func(context.Context) error { return nil }, nil)
}

func TestDoRetriesTransientThenSucceeds(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := idleBreaker(clk)
	var calls int
	elapsed, err := runDo(t, clk, b, func(context.Context) error {
		calls++
		if calls < 3 {
			return errTransient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	// Jitter bounds are 200 ms then 400 ms; the driver steps in 50 ms.
	if elapsed > 200*time.Millisecond+400*time.Millisecond+100*time.Millisecond {
		t.Fatalf("backoff %v exceeds the 200ms + 400ms jitter bounds", elapsed)
	}
	// The intermediate transient failures were not reported: 4 more must not open.
	for range 4 {
		b.Failure(false)
	}
	if s, _ := b.State(); s != Closed {
		t.Fatalf("state = %d, want Closed (Do reports only the final outcome)", s)
	}
}

func TestDoGivesUpAfterThreeAttemptsAndReportsOnce(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := idleBreaker(clk)
	var calls int
	_, err := runDo(t, clk, b, func(context.Context) error { calls++; return errTransient })
	if !errors.Is(err, errTransient) {
		t.Fatalf("Do = %v, want errTransient", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	// One failure was reported, so four more are needed to reach the threshold.
	for range 3 {
		b.Failure(false)
	}
	if s, _ := b.State(); s != Closed {
		t.Fatalf("state = %d, want Closed after 1+3 failures", s)
	}
	b.Failure(false)
	if s, _ := b.State(); s != Open {
		t.Fatalf("state = %d, want Open after 1+4 failures", s)
	}
}

func TestDoDoesNotRetryPermanent(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := idleBreaker(clk)
	var calls int
	_, err := runDo(t, clk, b, func(context.Context) error { calls++; return errPermanent })
	if !errors.Is(err, errPermanent) {
		t.Fatalf("Do = %v, want errPermanent", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if s, _ := b.State(); s != Open {
		t.Fatalf("state = %d, want Open after a permanent failure", s)
	}
}

func TestDoDoesNotRetryNotFoundAndKeepsBreakerClosed(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := idleBreaker(clk)
	var calls int
	_, err := runDo(t, clk, b, func(context.Context) error { calls++; return errMissing })
	if !errors.Is(err, errMissing) || calls != 1 {
		t.Fatalf("Do = %v after %d calls, want errMissing after 1", err, calls)
	}
	if s, _ := b.State(); s != Closed {
		t.Fatalf("state = %d, want Closed", s)
	}
}

func TestDoWaitsWhileBreakerOpen(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := idleBreaker(clk)
	b.Failure(true)
	var called atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		res <- Do(ctx, clk, classify, b, func(context.Context) error { called.Store(true); return nil })
	}()
	select {
	case <-res:
		t.Fatal("Do returned while the breaker was open")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v after cancel, want context.Canceled", err)
	}
	if called.Load() {
		t.Fatal("op ran while the breaker was open")
	}
}

func TestDoContextCancelIsNotReportedToBreaker(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := idleBreaker(clk)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 10 {
		err := Do(ctx, clk, classify, b, func(context.Context) error { cancel(); return context.Canceled })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Do = %v, want context.Canceled", err)
		}
	}
	if s, _ := b.State(); s != Closed {
		t.Fatalf("state = %d, want Closed (shutdown is not a storage failure)", s)
	}
}
