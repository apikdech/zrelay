// Package clock abstracts wall time so that code with timeouts, backoff and
// timers can be tested deterministically with a Fake clock.
package clock

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

// Clock is the source of time for zrelay components.
type Clock interface {
	// Now returns the current wall time in UTC.
	Now() time.Time
	// NewTimer returns a Timer that sends the time on C after d.
	NewTimer(d time.Duration) Timer
	// Sleep blocks for d or until ctx is done, returning ctx.Err() in the
	// latter case.
	Sleep(ctx context.Context, d time.Duration) error
}

// Timer is a one-shot timer. Stop and Reset discard any value that fired
// but was not yet received, matching time.Timer semantics since Go 1.23.
type Timer interface {
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether the timer
	// was pending.
	Stop() bool
	// Reset re-arms the timer to fire d after the current time.
	Reset(d time.Duration)
}

// Real is the Clock backed by the system clock.
var Real Clock = realClock{}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

func (c realClock) Sleep(ctx context.Context, d time.Duration) error { return sleep(ctx, c, d) }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time   { return r.t.C }
func (r realTimer) Stop() bool            { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) { r.t.Reset(d) }

func sleep(ctx context.Context, c Clock, d time.Duration) error {
	if err := ctx.Err(); err != nil || d <= 0 {
		return err
	}
	t := c.NewTimer(d)
	select {
	case <-ctx.Done():
		t.Stop()
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// Fake is a manually driven Clock for tests. Time moves only when Advance is
// called. It is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	changed *sync.Cond // signalled when the set of pending timers grows
	now     time.Time
	pending []*fakeTimer // sorted by deadline, then creation order
	seq     uint64
}

// NewFake returns a Fake clock whose current time is start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.changed = sync.NewCond(&f.mu)
	return f
}

// Now returns the fake current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer returns a Timer that fires once the fake time reaches Now()+d.
// A timer with d <= 0 fires immediately.
func (f *Fake) NewTimer(d time.Duration) Timer {
	t := &fakeTimer{f: f, c: make(chan time.Time, 1)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schedule(t, d)
	return t
}

// Sleep blocks until the fake time has advanced by d or ctx is done.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error { return sleep(ctx, f, d) }

// Advance moves the fake time forward by d, firing every timer that falls
// due, in deadline order. Each timer receives its own deadline as its value.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for len(f.pending) > 0 && !f.pending[0].deadline.After(target) {
		t := f.pending[0]
		f.pending = f.pending[1:]
		t.armed = false
		if t.deadline.After(f.now) {
			f.now = t.deadline
		}
		t.send(t.deadline)
	}
	f.now = target
}

// Pending returns the number of timers waiting to fire, including those
// created by Sleep.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// BlockUntil blocks until at least n timers are pending. Tests use it to
// wait until the code under test has armed its timers before calling
// Advance.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.pending) < n {
		f.changed.Wait()
	}
}

// schedule arms t to fire d after the current time. f.mu must be held.
func (f *Fake) schedule(t *fakeTimer, d time.Duration) {
	t.deadline = f.now.Add(d)
	if d <= 0 {
		t.send(t.deadline)
		return
	}
	f.seq++
	t.seq = f.seq
	t.armed = true
	i, _ := slices.BinarySearchFunc(f.pending, t, func(a, b *fakeTimer) int {
		if c := a.deadline.Compare(b.deadline); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	f.pending = slices.Insert(f.pending, i, t)
	f.changed.Broadcast()
}

// unschedule removes t from the pending list and drains any unread value.
// It reports whether t was pending. f.mu must be held.
func (f *Fake) unschedule(t *fakeTimer) bool {
	select {
	case <-t.c:
	default:
	}
	if !t.armed {
		return false
	}
	t.armed = false
	f.pending = slices.DeleteFunc(f.pending, func(p *fakeTimer) bool { return p == t })
	return true
}

type fakeTimer struct {
	f        *Fake
	c        chan time.Time
	deadline time.Time
	seq      uint64
	armed    bool // guarded by f.mu
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	return t.f.unschedule(t)
}

func (t *fakeTimer) Reset(d time.Duration) {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.unschedule(t)
	t.f.schedule(t, d)
}

// send delivers v without blocking; the channel holds at most one value.
func (t *fakeTimer) send(v time.Time) {
	select {
	case t.c <- v:
	default:
	}
}
