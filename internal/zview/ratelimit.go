package zview

import (
	"sync"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

const (
	// authFailLimit is how many failed authentications one client IP may
	// make per authFailWindow before it is answered with 429 (ui-spec §3.2).
	authFailLimit  = 10
	authFailWindow = time.Minute

	// maxLimiterKeys bounds the limiter's memory against a flood of source
	// addresses.
	maxLimiterKeys = 4096
)

// failureLimiter counts failures per key in fixed windows. It is safe for
// concurrent use.
type failureLimiter struct {
	clk    clock.Clock
	limit  int
	window time.Duration

	mu sync.Mutex
	m  map[string]*failWindow
}

type failWindow struct {
	start time.Time
	n     int
}

func newFailureLimiter(clk clock.Clock, limit int, window time.Duration) *failureLimiter {
	return &failureLimiter{clk: clk, limit: limit, window: window, m: map[string]*failWindow{}}
}

// fail records one failure for key and reports whether it is still within
// the limit. The (limit+1)th failure in a window reports false.
func (l *failureLimiter) fail(key string) bool {
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.m[key]
	if w == nil || now.Sub(w.start) >= l.window {
		if w == nil && len(l.m) >= maxLimiterKeys {
			l.pruneLocked(now)
		}
		w = &failWindow{start: now}
		l.m[key] = w
	}
	w.n++
	return w.n <= l.limit
}

// pruneLocked drops expired windows. If the table is still full the address
// flood is real: it is cleared rather than grown, which forgets some counts
// but keeps memory bounded.
func (l *failureLimiter) pruneLocked(now time.Time) {
	for k, w := range l.m {
		if now.Sub(w.start) >= l.window {
			delete(l.m, k)
		}
	}
	if len(l.m) >= maxLimiterKeys {
		clear(l.m)
	}
}

func (l *failureLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}
