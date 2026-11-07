package egress

import (
	"testing"
)

// TestInFlightNamesTheKeysOfClipsBeingWritten: recovery must never touch the
// video or staging sidecar of a clip this process is still writing.
func TestInFlightNamesTheKeysOfClipsBeingWritten(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	c.write(t, make([]byte, 100))

	for _, k := range []string{c.pc.Keys.Video, c.pc.Keys.StagingSidecar} {
		if !r.e.InFlight(k) {
			t.Errorf("InFlight(%q) = false for a clip being written", k)
		}
	}
	if r.e.InFlight("clips/front/2026/11/06/145900000_OTHER.zrec") {
		t.Error("InFlight is true for a key of no clip")
	}

	c.finish()
	eventually(t, "the clip to be committed", func() bool { return st.count("commit") == 1 })
	eventually(t, "the clip to leave the in-flight set", func() bool { return !r.e.InFlight(c.pc.Keys.Video) })
}
