package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/egress"
)

// healthApp is an app with only what healthErr reads: a clock and a breaker
// that is not running its probe loop.
func healthApp(t *testing.T) (*app, *clock.Fake, *egress.Breaker) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	br := egress.NewBreaker(egress.BreakerConfig{FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Minute},
		clk, func(context.Context) error { return nil }, nil)
	return &app{clk: clk, breaker: br}, clk, br
}

// /healthz is unhealthy only once the breaker has been open longer than five
// minutes (design §6.3); a short outage must not restart-loop the container.
func TestHealthErrBreakerOpenLongerThanFiveMinutes(t *testing.T) {
	a, clk, br := healthApp(t)
	if err := a.healthErr(); err != nil {
		t.Fatalf("closed breaker: %v", err)
	}

	br.Failure(true) // a permanent failure opens at once
	if st, _ := br.State(); st != egress.Open {
		t.Fatalf("breaker state = %v, want open", st)
	}
	clk.Advance(5 * time.Minute)
	if err := a.healthErr(); err != nil {
		t.Errorf("open for exactly 5 minutes: %v, want healthy", err)
	}
	clk.Advance(time.Second)
	err := a.healthErr()
	if err == nil {
		t.Fatal("open for more than 5 minutes: healthy, want unhealthy")
	}
	if !strings.Contains(err.Error(), "breaker") {
		t.Errorf("error %q should name the breaker", err)
	}
}

// The supervisor is not running once shutdown began.
func TestHealthErrWhileShuttingDown(t *testing.T) {
	a, _, _ := healthApp(t)
	a.stopping.Store(true)
	if err := a.healthErr(); err == nil {
		t.Fatal("healthy while shutting down")
	}
}
