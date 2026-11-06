package egress

import (
	"fmt"
	"testing"
	"time"
)

// breakerFailures returns the breaker's consecutive-failure count.
func breakerFailures(b *Breaker) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

// A request that has gone unanswered for stallReport when the backlog policy
// cancels it is a storage failure: under sustained overload the oldest
// finalized clip (the one a worker is draining) is evicted before any request
// times out, so without this an outage would never reach FailureThreshold.
// A request cancelled sooner proves nothing and is not reported.
func TestEvictionCancelsStalledRequestAndReportsBreakerFailure(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, st *fakeStore, c *testClip)
		entry func(st *fakeStore) bool
	}{
		{
			name:  "staging put",
			setup: func(t *testing.T, st *fakeStore, c *testClip) { c.finish() },
			entry: func(st *fakeStore) bool { return st.hungPuts >= 1 },
		},
		{
			name: "part upload",
			setup: func(t *testing.T, st *fakeStore, c *testClip) {
				eventually(t, "created", func() bool { return st.obj(c.pc.Keys.Video) != nil })
				st.mu.Lock()
				st.hangWrites = true
				st.mu.Unlock()
				c.write(t, make([]byte, 100))
				c.finish()
			},
			entry: func(st *fakeStore) bool { return st.hungWrites >= 1 },
		},
	}
	for _, tc := range cases {
		for _, stalled := range []bool{true, false} {
			name, want, wait := tc.name+"/short", 0, time.Second
			if stalled {
				name, want, wait = tc.name+"/stalled", 1, stallReport+time.Second
			}
			t.Run(name, func(t *testing.T) {
				st := newFakeStore()
				st.durableStep = 1 << 30
				st.hangPuts = tc.name == "staging put"
				r := newEgressRig(t, 1, 1<<30, st)
				c := r.begin("front", "A")
				tc.setup(t, st, c)
				eventually(t, "the request to be in flight", func() bool {
					st.mu.Lock()
					defer st.mu.Unlock()
					return tc.entry(st)
				})
				r.clk.Advance(wait)
				c.w.(*Stream).Evict() // what the accountant does to the oldest finalized clip
				eventually(t, "the worker to give up the clip", func() bool {
					r.e.mu.Lock()
					defer r.e.mu.Unlock()
					return len(r.e.jobs) == 0
				})
				if got := breakerFailures(r.br); got != want {
					t.Fatalf("breaker failures after evicting a request pending for %v = %d, want %d", wait, got, want)
				}
			})
		}
	}
}

// Stalled evictions reaching the breaker threshold open it, which is what
// parks the workers during a real outage.
func TestStalledEvictionsOpenTheBreaker(t *testing.T) {
	st := newFakeStore()
	st.hangPuts = true
	r := newEgressRig(t, 1, 1<<30, st)
	for i := range 5 { // FailureThreshold in the rig
		c := r.begin("front", fmt.Sprintf("C%d", i))
		c.finish()
		eventually(t, "the request to be in flight", func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return st.hungPuts >= i+1
		})
		r.clk.Advance(stallReport + time.Second)
		c.w.(*Stream).Evict()
	}
	eventually(t, "the breaker to open", func() bool {
		s, _ := r.br.State()
		return s == Open
	})
}
