package events

import (
	"slices"
	"sync"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

// Tracker holds a camera's trigger state per kind (relay-spec §4.4). The
// camera is triggered while any kind is active. It is safe for concurrent
// use.
type Tracker struct {
	timeout time.Duration
	clk     clock.Clock
	changes chan struct{}

	mu sync.Mutex
	// lastActive holds each active kind and when it was last seen active.
	lastActive map[Kind]time.Time
}

// NewTracker returns a Tracker that forces a kind inactive timeout after its
// last active signal.
func NewTracker(timeout time.Duration, clk clock.Clock) *Tracker {
	return &Tracker{timeout: timeout, clk: clk, changes: make(chan struct{}, 1), lastActive: map[Kind]time.Time{}}
}

// Apply records s. An active signal activates its kind, or refreshes its
// timeout if already active. An inactive signal deactivates its kind,
// except an Initialized one, which is ignored.
func (t *Tracker) Apply(s Signal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, was := t.lastActive[s.Kind]
	switch {
	case s.Active:
		t.lastActive[s.Kind] = t.clk.Now()
		if !was {
			t.notify()
		}
	case s.Initialized:
	case was:
		delete(t.lastActive, s.Kind)
		t.notify()
	}
}

// Expire forces inactive every kind whose last active signal is at least
// the timeout old.
func (t *Tracker) Expire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clk.Now()
	for k, at := range t.lastActive {
		if now.Sub(at) >= t.timeout {
			delete(t.lastActive, k)
			t.notify()
		}
	}
}

// ResetAll forces every kind inactive.
func (t *Tracker) ResetAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lastActive) > 0 {
		clear(t.lastActive)
		t.notify()
	}
}

// Triggered reports whether any kind is active.
func (t *Tracker) Triggered() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.lastActive) > 0
}

// Active returns the active kinds, sorted.
func (t *Tracker) Active() []Kind {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Kind, 0, len(t.lastActive))
	for k := range t.lastActive {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Changes receives a value after the triggered state or the set of active
// kinds changes. Changes coalesce: one pending value stands for any number.
func (t *Tracker) Changes() <-chan struct{} { return t.changes }

// notify signals a change without blocking. t.mu must be held.
func (t *Tracker) notify() {
	select {
	case t.changes <- struct{}{}:
	default:
	}
}
