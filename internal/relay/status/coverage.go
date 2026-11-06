// Package status publishes the relay heartbeat (status/relay.json) and the
// hourly coverage objects (status/coverage/...) of Contract §7.5.
package status

import (
	"slices"
	"sync"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
)

// span is a half-open ready interval [start, end).
type span struct{ start, end time.Time }

// CoverageTracker accumulates, for one camera, the intervals during which an
// event would have been captured (RTSP buffer primed and event subscription
// active) and the clips dropped. It is memory-only and safe for concurrent
// use. Data is released by TakeHour, which expects hours to be taken in
// ascending order.
type CoverageTracker struct {
	clk clock.Clock

	mu        sync.Mutex
	closed    []span // ascending, non-overlapping, non-touching
	open      bool   // currently ready
	openStart time.Time
	last      time.Time // latest SetReady transition; later calls never go backwards
	drops     []sidecar.Dropped
}

// NewCoverageTracker returns a tracker that is not ready. clk bounds a
// still-open interval when an hour is taken before it has ended.
func NewCoverageTracker(clk clock.Clock) *CoverageTracker {
	return &CoverageTracker{clk: clk}
}

// SetReady records that the camera became ready (true) or not ready (false)
// at the given time. Repeating the current state is a no-op. A transition
// timestamped before the previous one is moved forward to it.
func (t *CoverageTracker) SetReady(ready bool, at time.Time) {
	at = at.UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	if at.Before(t.last) {
		at = t.last
	}
	t.last = at
	switch {
	case ready && !t.open:
		t.open = true
		t.openStart = at
		// An interval that ended exactly now continues instead of starting a
		// second, touching one.
		if n := len(t.closed); n > 0 && t.closed[n-1].end.Equal(at) {
			t.openStart = t.closed[n-1].start
			t.closed = t.closed[:n-1]
		}
	case !ready && t.open:
		t.open = false
		if at.After(t.openStart) {
			t.closed = append(t.closed, span{t.openStart, at})
		}
	}
}

// Drop records a clip dropped at the given time. reason is one of
// stream_lost, backlog or shutdown; approxMs estimates the footage lost.
func (t *CoverageTracker) Drop(at time.Time, reason string, approxMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.drops = append(t.drops, sidecar.Dropped{At: sidecar.Time{Time: at.UTC()}, Reason: reason, ApproxMs: approxMs})
}

// TakeHour returns the coverage of the UTC hour containing hour, without
// CameraID, and removes it from the tracker. Ready intervals are clamped to
// the hour and merged; a still-open interval ends at the hour end, or at the
// current time when the hour has not ended yet (the partial hour written on
// shutdown). Ready and Dropped are never nil.
func (t *CoverageTracker) TakeHour(hour time.Time) sidecar.Coverage {
	hour = hour.UTC().Truncate(time.Hour)
	end := hour.Add(time.Hour)
	now := t.clk.Now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()

	ready := []sidecar.Interval{}
	add := func(s, e time.Time) {
		if s.Before(hour) {
			s = hour
		}
		if e.After(end) {
			e = end
		}
		if s.Before(e) {
			ready = append(ready, sidecar.Interval{{Time: s}, {Time: e}})
		}
	}
	for _, sp := range t.closed {
		add(sp.start, sp.end)
	}
	if t.open {
		add(t.openStart, now)
	}

	// Release everything before the cut: the hour end, or now for a partial
	// hour. What remains of a straddling interval starts at the cut.
	cut := end
	if now.Before(cut) {
		cut = now
	}
	if cut.After(hour) {
		kept := t.closed[:0]
		for _, sp := range t.closed {
			if !sp.end.After(cut) {
				continue
			}
			if sp.start.Before(cut) {
				sp.start = cut
			}
			kept = append(kept, sp)
		}
		t.closed = kept
		if t.open && t.openStart.Before(cut) {
			t.openStart = cut
		}
	}

	dropped := []sidecar.Dropped{}
	keptDrops := t.drops[:0]
	for _, d := range t.drops {
		switch {
		case d.At.Before(hour):
			// Stale: its hour was never taken. Discard.
		case d.At.Before(end):
			dropped = append(dropped, d)
		default:
			keptDrops = append(keptDrops, d)
		}
	}
	t.drops = keptDrops
	slices.SortStableFunc(dropped, func(a, b sidecar.Dropped) int { return a.At.Compare(b.At.Time) })

	return sidecar.Coverage{
		Schema:    sidecar.CoverageSchema,
		HourStart: sidecar.Time{Time: hour},
		Ready:     ready,
		Dropped:   dropped,
	}
}
