package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

// TestBreakerReportsThePermanentCause: a permanent failure (a rejected
// credential, a missing bucket) opens the breaker at once; design §6.3 wants
// it logged once per transition with its cause, which the breaker's state
// callback cannot carry. OnPermanent is called exactly when a permanent
// failure opens the breaker.
func TestBreakerReportsThePermanentCause(t *testing.T) {
	var got []error
	clk := clock.NewFake(epoch)
	b := NewBreaker(BreakerConfig{
		FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Minute,
		OnPermanent: func(err error) { got = append(got, err) },
	}, clk, func(context.Context) error { return nil }, nil)

	boom := errors.New("AccessDenied: Access Denied")
	b.FailureErr(errors.New("timeout"), false) // transient, below the threshold
	if len(got) != 0 {
		t.Fatalf("OnPermanent called for a transient failure: %v", got)
	}
	b.FailureErr(boom, true)
	if len(got) != 1 || !errors.Is(got[0], boom) {
		t.Fatalf("OnPermanent calls = %v, want one with %v", got, boom)
	}
	if st, _ := b.State(); st != Open {
		t.Fatalf("state = %v, want Open", st)
	}
	b.FailureErr(boom, true) // already open: a late result of an in-flight request
	if len(got) != 1 {
		t.Errorf("OnPermanent called again while the breaker was open: %v", got)
	}
}

// TestDoReportsAPermanentErrorToTheBreaker: the retry wrapper hands the
// permanent error itself to the breaker.
func TestDoReportsAPermanentErrorToTheBreaker(t *testing.T) {
	var got []error
	clk := clock.NewFake(epoch)
	b := NewBreaker(BreakerConfig{
		OnPermanent: func(err error) { got = append(got, err) },
	}, clk, func(context.Context) error { return nil }, nil)

	err := Do(context.Background(), clk, classify, b, func(context.Context) error { return errPermanent })
	if !errors.Is(err, errPermanent) {
		t.Fatalf("Do = %v", err)
	}
	if len(got) != 1 || !errors.Is(got[0], errPermanent) {
		t.Errorf("OnPermanent calls = %v, want one with the operation's error", got)
	}
}
