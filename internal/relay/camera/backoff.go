package camera

import (
	"math/rand/v2"
	"time"
)

// BackoffConfig is the reconnect backoff: full-jitter exponential from Min
// to Max, restarted after a session stays up for ResetAfter (design §4.2).
// Zero fields default to 1 s, 60 s and 5 min.
type BackoffConfig struct {
	Min, Max, ResetAfter time.Duration
}

const (
	defaultBackoffMin   = time.Second
	defaultBackoffMax   = 60 * time.Second
	defaultBackoffReset = 5 * time.Minute
)

func (c BackoffConfig) withDefaults() BackoffConfig {
	if c.Min <= 0 {
		c.Min = defaultBackoffMin
	}
	if c.Max <= 0 {
		c.Max = defaultBackoffMax
	}
	c.Max = max(c.Max, c.Min)
	if c.ResetAfter <= 0 {
		c.ResetAfter = defaultBackoffReset
	}
	return c
}

// backoff yields reconnect delays. It is not safe for concurrent use.
type backoff struct {
	cfg BackoffConfig
	// rnd returns a uniform value in [0, n).
	rnd func(n int64) int64
	n   int // failures since the last reset
}

func newBackoff(cfg BackoffConfig, rnd func(n int64) int64) *backoff {
	if rnd == nil {
		rnd = rand.Int64N
	}
	return &backoff{cfg: cfg.withDefaults(), rnd: rnd}
}

// next returns the delay after a failure that ended ranFor after its
// start: uniform in [0, min(Max, Min*2^n)] for the n-th consecutive
// failure, with n back to 0 when ranFor reached ResetAfter.
func (b *backoff) next(ranFor time.Duration) time.Duration {
	if ranFor >= b.cfg.ResetAfter {
		b.n = 0
	}
	limit := b.cfg.Min
	for i := 0; i < b.n && limit < b.cfg.Max; i++ {
		limit *= 2
	}
	limit = min(limit, b.cfg.Max)
	b.n++
	return time.Duration(b.rnd(int64(limit) + 1))
}
