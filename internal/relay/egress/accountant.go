package egress

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/sidecar"
)

// DropBacklog is the reason passed to the accountant's onDrop callback and
// recorded in coverage and zrelay_clips_dropped_total{reason}.
const DropBacklog = "backlog"

// Clip is one upload unit shared between the clip lifecycle (Task 26) and
// the accountant. Finalized is set by the owner once the clip's last byte
// has been written; the clip stays registered until it is committed.
type Clip struct {
	Camera    string
	Keys      sidecar.ClipKeySet
	Staging   []byte
	Final     func() ([]byte, error)
	Stream    *Stream
	Finalized atomic.Bool
	// Committed is set by the owner as soon as the video object is
	// committed. A committed clip is no longer backlog: the accountant never
	// evicts it, even while its stream still holds bytes.
	Committed atomic.Bool
	Created   time.Time
}

// Accountant bounds the bytes held by all registered streams. On every
// stream growth over the limit it evicts the oldest finalized-but-uncommitted
// clip (repeating while still over), else asks for the in-progress clip with
// the largest backlog to be force-finalized.
type Accountant struct {
	limit         int64
	onDrop        func(c *Clip, reason string)
	forceFinalize func(camera string)

	mu     sync.Mutex
	clips  map[*Clip]struct{}
	forced map[*Clip]struct{} // force-finalize requested, not yet finalized
}

// NewAccountant returns an accountant with a limit in bytes. onDrop is called
// after a clip's stream was evicted; forceFinalize asks the camera's
// recorder to finalize its current clip. Either may be nil. Both are called
// without internal locks held.
func NewAccountant(limit int64, onDrop func(c *Clip, reason string), forceFinalize func(camera string)) *Accountant {
	return &Accountant{
		limit:         limit,
		onDrop:        onDrop,
		forceFinalize: forceFinalize,
		clips:         make(map[*Clip]struct{}),
		forced:        make(map[*Clip]struct{}),
	}
}

// Register starts accounting c.Stream and enforces on every growth of it.
func (a *Accountant) Register(c *Clip) {
	if c == nil || c.Stream == nil {
		return
	}
	a.mu.Lock()
	a.clips[c] = struct{}{}
	a.mu.Unlock()
	c.Stream.mu.Lock()
	c.Stream.onGrow = a.Enforce
	c.Stream.mu.Unlock()
}

// Unregister stops accounting c (committed, dropped or abandoned).
func (a *Accountant) Unregister(c *Clip) {
	if c == nil {
		return
	}
	a.mu.Lock()
	delete(a.clips, c)
	delete(a.forced, c)
	a.mu.Unlock()
	if c.Stream != nil {
		c.Stream.mu.Lock()
		c.Stream.onGrow = nil
		c.Stream.mu.Unlock()
	}
}

// Total returns the bytes currently buffered across registered streams.
func (a *Accountant) Total() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totalLocked()
}

func (a *Accountant) totalLocked() int64 {
	var t int64
	for c := range a.clips {
		t += c.Stream.Buffered()
	}
	return t
}

// Enforce applies the backlog policy now. Streams call it on every Write;
// owners call it after finalizing a clip they were asked to force-finalize,
// because no further Write may follow.
func (a *Accountant) Enforce() {
	var dropped []*Clip
	var force string
	doForce := false

	a.mu.Lock()
	for a.totalLocked() > a.limit {
		if oldest := a.oldestFinalizedLocked(); oldest != nil {
			if oldest.Stream.Evict() {
				dropped = append(dropped, oldest)
			}
			delete(a.forced, oldest)
			continue
		}
		// A force-finalize still pending: wait for its owner to finalize.
		pending := false
		for c := range a.forced {
			if !c.Finalized.Load() {
				pending = true
				break
			}
		}
		if pending {
			break
		}
		if c := a.largestInProgressLocked(); c != nil {
			a.forced[c] = struct{}{}
			force, doForce = c.Camera, true
		}
		break
	}
	a.mu.Unlock()

	for _, c := range dropped {
		if a.onDrop != nil {
			a.onDrop(c, DropBacklog)
		}
	}
	if doForce && a.forceFinalize != nil {
		a.forceFinalize(force)
	}
}

// oldestFinalizedLocked returns the oldest finalized clip that still holds
// bytes and has not been evicted.
func (a *Accountant) oldestFinalizedLocked() *Clip {
	var best *Clip
	for c := range a.clips {
		if !c.Finalized.Load() || c.Committed.Load() || c.Stream.isEvicted() || c.Stream.Buffered() == 0 {
			continue
		}
		if best == nil || c.Created.Before(best.Created) {
			best = c
		}
	}
	return best
}

// largestInProgressLocked returns the unfinalized clip with the largest
// backlog that has not already been asked to finalize.
func (a *Accountant) largestInProgressLocked() *Clip {
	var best *Clip
	var bestN int64
	for c := range a.clips {
		if c.Finalized.Load() || c.Stream.isEvicted() {
			continue
		}
		if _, done := a.forced[c]; done {
			continue
		}
		if n := c.Stream.Buffered(); best == nil || n > bestN {
			best, bestN = c, n
		}
	}
	return best
}
