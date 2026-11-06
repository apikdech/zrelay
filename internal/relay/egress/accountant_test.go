package egress_test

import (
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/egress"
)

var base = time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)

type recorder struct {
	mu     sync.Mutex
	drops  []*egress.Clip
	why    []string
	forced []string
}

func (r *recorder) onDrop(c *egress.Clip, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drops = append(r.drops, c)
	r.why = append(r.why, reason)
}

func (r *recorder) forceFinalize(cam string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forced = append(r.forced, cam)
}

func newClip(cam string, age time.Duration, finalized bool) *egress.Clip {
	c := &egress.Clip{Camera: cam, Stream: egress.NewStream(), Created: base.Add(age)}
	c.Finalized.Store(finalized)
	return c
}

func fill(t *testing.T, c *egress.Clip, n int) {
	t.Helper()
	if _, err := c.Stream.Write(make([]byte, n)); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func evicted(c *egress.Clip) bool {
	select {
	case <-c.Stream.Evicted():
		return true
	default:
		return false
	}
}

func TestAccountantUnderLimitNoAction(t *testing.T) {
	var r recorder
	a := egress.NewAccountant(1000, r.onDrop, r.forceFinalize)
	c1 := newClip("a", 0, true)
	c2 := newClip("b", time.Second, false)
	a.Register(c1)
	a.Register(c2)
	fill(t, c1, 400)
	fill(t, c2, 600) // exactly at the limit is not over it
	if got := a.Total(); got != 1000 {
		t.Fatalf("Total = %d, want 1000", got)
	}
	if len(r.drops) != 0 || len(r.forced) != 0 || evicted(c1) || evicted(c2) {
		t.Fatalf("unexpected action: drops=%v forced=%v", r.why, r.forced)
	}
}

func TestAccountantEvictsOldestFinalizedFirst(t *testing.T) {
	var r recorder
	a := egress.NewAccountant(1000, r.onDrop, r.forceFinalize)
	newest := newClip("a", 2*time.Second, true)
	oldest := newClip("b", 0, true)
	middle := newClip("c", time.Second, true)
	live := newClip("d", 3*time.Second, false)
	for _, c := range []*egress.Clip{newest, oldest, middle, live} {
		a.Register(c)
	}
	fill(t, newest, 300)
	fill(t, oldest, 300)
	fill(t, middle, 300)
	fill(t, live, 100) // total 1000: not over
	if len(r.drops) != 0 {
		t.Fatalf("dropped early: %v", r.why)
	}
	fill(t, live, 100) // 1100: over by 100 -> evict oldest only
	if len(r.drops) != 1 || r.drops[0] != oldest || r.why[0] != "backlog" {
		t.Fatalf("drops = %v %v, want oldest/backlog", r.drops, r.why)
	}
	if !evicted(oldest) || evicted(middle) || evicted(newest) || evicted(live) {
		t.Fatal("wrong clips evicted")
	}
	if got := a.Total(); got != 800 {
		t.Fatalf("Total = %d, want 800", got)
	}
	if len(r.forced) != 0 {
		t.Fatalf("forceFinalize called while finalized clips existed: %v", r.forced)
	}
	fill(t, live, 500) // 1300 -> evict middle (300) -> 1000, not over
	if len(r.drops) != 2 || r.drops[1] != middle {
		t.Fatalf("second drop = %v", r.drops)
	}
	fill(t, live, 400) // 1400 -> evict newest (300) -> 1100, still over; none left
	if len(r.drops) != 3 || r.drops[2] != newest {
		t.Fatalf("third drop = %v", r.drops)
	}
	if len(r.forced) != 1 || r.forced[0] != "d" {
		t.Fatalf("forced = %v, want [d]", r.forced)
	}
}

func TestAccountantForcesLargestInProgressWhenNoFinalized(t *testing.T) {
	var r recorder
	a := egress.NewAccountant(1000, r.onDrop, r.forceFinalize)
	small := newClip("small", 0, false)
	big := newClip("big", time.Second, false)
	mid := newClip("mid", 2*time.Second, false)
	for _, c := range []*egress.Clip{small, big, mid} {
		a.Register(c)
	}
	fill(t, small, 100)
	fill(t, big, 600)
	fill(t, mid, 300)
	if len(r.forced) != 0 {
		t.Fatalf("forced early: %v", r.forced)
	}
	fill(t, mid, 100) // 1100
	if len(r.forced) != 1 || r.forced[0] != "big" {
		t.Fatalf("forced = %v, want [big]", r.forced)
	}
	if len(r.drops) != 0 || evicted(big) {
		t.Fatal("in-progress clip must be finalized by its owner, not evicted")
	}
}

func TestAccountantForceFinalizeNotRepeatedWhilePending(t *testing.T) {
	var r recorder
	a := egress.NewAccountant(100, r.onDrop, r.forceFinalize)
	c := newClip("cam", 0, false)
	a.Register(c)
	fill(t, c, 150)
	fill(t, c, 10)
	fill(t, c, 10)
	if len(r.forced) != 1 {
		t.Fatalf("forced = %v, want exactly one call while pending", r.forced)
	}
}

func TestAccountantEnforceAfterFinalizeEvicts(t *testing.T) {
	var r recorder
	a := egress.NewAccountant(100, r.onDrop, r.forceFinalize)
	c := newClip("cam", 0, false)
	a.Register(c)
	fill(t, c, 150)
	c.Finalized.Store(true) // owner finished the forced finalize
	a.Enforce()
	if len(r.drops) != 1 || r.drops[0] != c || !evicted(c) {
		t.Fatalf("drops = %v", r.why)
	}
}

func TestAccountantUnregisterAndRelease(t *testing.T) {
	var r recorder
	a := egress.NewAccountant(1000, r.onDrop, r.forceFinalize)
	c1 := newClip("a", 0, true)
	c2 := newClip("b", time.Second, true)
	a.Register(c1)
	a.Register(c2)
	fill(t, c1, 500)
	fill(t, c2, 400)
	c1.Stream.Release(300) // durable bytes leave the backlog
	if got := a.Total(); got != 600 {
		t.Fatalf("Total after Release = %d, want 600", got)
	}
	a.Unregister(c1)
	if got := a.Total(); got != 400 {
		t.Fatalf("Total after Unregister = %d, want 400", got)
	}
	fill(t, c1, 5000) // unregistered streams no longer count or trigger
	if len(r.drops) != 0 || len(r.forced) != 0 {
		t.Fatalf("unregistered clip triggered action")
	}
}

func TestAccountantNilCallbacksConcurrent(t *testing.T) {
	a := egress.NewAccountant(1000, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		c := newClip("c", time.Duration(i)*time.Second, true)
		a.Register(c)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if _, err := c.Stream.Write(make([]byte, 50)); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	if a.Total() > 1000 {
		t.Fatalf("Total = %d, accountant did not enforce", a.Total())
	}
}
