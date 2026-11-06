// Package ring is the per-camera pre-roll buffer (relay-spec §4.3): a store
// of whole GOPs, IDR-aligned, that a clip takes over by reference on open.
package ring

import (
	"sync"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

// GOP is one group of pictures: an IDR access unit and the non-IDR units
// after it, in decode order. Start is the IDR's Wall time, StartDTS its DTS
// and ParamGen its parameter-set generation. Bytes is the sum of AU Bytes.
type GOP struct {
	AUs      []*camera.AccessUnit
	Start    time.Time
	StartDTS int64
	Bytes    int64
	ParamGen uint32
}

// Ring holds the retained GOPs of one camera. The last GOP is open and grows
// until the next IDR. It is safe for one pushing goroutine and any number of
// Retained callers.
type Ring struct {
	preRoll    time.Duration
	capBytes   int64
	onTruncate func()

	mu    sync.Mutex
	gops  []*GOP // oldest first; the last one is open when open is true
	open  bool
	bytes int64
	hint  int // AU count of the last closed GOP, to size the next slice
}

// New returns a ring that keeps pre-roll worth of whole GOPs within capBytes.
// onTruncate (may be nil) runs once per GOP evicted by the hard cap, so the
// caller can count zrelay_preroll_truncated_total.
func New(preRoll time.Duration, capBytes int64, onTruncate func()) *Ring {
	return &Ring{preRoll: preRoll, capBytes: capBytes, onTruncate: onTruncate}
}

// Push adds au. An IDR closes the open GOP and starts a new one; a non-IDR
// joins the open GOP, or is dropped when there is none (before the first IDR,
// after Reset, or after the hard cap evicted the open GOP). Then the
// retention rule runs with au.Wall as now, and the hard cap after it.
func (r *Ring) Push(au *camera.AccessUnit) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case au.IDR:
		if r.open {
			r.hint = len(r.gops[len(r.gops)-1].AUs)
		}
		aus := make([]*camera.AccessUnit, 0, max(r.hint, 1))
		r.gops = append(r.gops, &GOP{
			AUs:      append(aus, au),
			Start:    au.Wall,
			StartDTS: au.DTS,
			Bytes:    int64(au.Bytes),
			ParamGen: au.ParamGen,
		})
		r.open = true
	case r.open:
		g := r.gops[len(r.gops)-1]
		g.AUs = append(g.AUs, au)
		g.Bytes += int64(au.Bytes)
	default:
		return
	}
	r.bytes += int64(au.Bytes)

	// Keep the newest GOP starting at or before now−preRoll, and all after it.
	cut := au.Wall.Add(-r.preRoll)
	for len(r.gops) >= 2 && !r.gops[1].Start.After(cut) {
		r.evictOldest()
	}
	for r.bytes > r.capBytes && len(r.gops) > 0 {
		r.evictOldest()
		if r.onTruncate != nil {
			r.onTruncate()
		}
	}
}

// evictOldest drops the oldest GOP. Clips that hold it keep it alive.
func (r *Ring) evictOldest() {
	r.bytes -= r.gops[0].Bytes
	r.gops[0] = nil
	r.gops = r.gops[1:]
	if len(r.gops) == 0 {
		r.open = false
	}
}

// Retained returns the retained GOPs, oldest first, the open one last. The
// AUs are shared with the ring, never copied. Closed GOPs are returned as
// is; the open GOP is returned as a snapshot of the AUs pushed so far, so
// later pushes never change what the caller holds.
func (r *Ring) Retained() []*GOP {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*GOP, len(r.gops))
	copy(out, r.gops)
	if r.open {
		g := *out[len(out)-1]
		g.AUs = g.AUs[:len(g.AUs):len(g.AUs)]
		out[len(out)-1] = &g
	}
	return out
}

// Bytes returns the running byte total of the retained GOPs.
func (r *Ring) Bytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

// Reset drops every GOP, as on EventLost or a codec change. Non-IDR units are
// dropped until the next IDR.
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.gops)
	r.gops = r.gops[:0]
	r.open = false
	r.bytes = 0
	r.hint = 0
}
