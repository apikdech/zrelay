package app_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/sidecar"
)

// coverageObjects returns the camera's published coverage objects under the
// local driver's base directory.
func coverageObjects(t *testing.T, base, camera string) []sidecar.Coverage {
	t.Helper()
	var out []sidecar.Coverage
	for _, p := range files(base, func(rel string) bool {
		return strings.Contains(rel, "status/coverage/") && strings.Contains(rel, camera) && strings.HasSuffix(rel, ".json")
	}) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var c sidecar.Coverage
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, c)
	}
	return out
}

// TestCoverageExcludesBacklogSuppression: while the backlog policy keeps a
// camera Suppressed it ignores triggers and records nothing, so Contract
// section 7.5's ready (the intervals when an event would have been captured)
// must not list that time, even though RTSP and ONVIF stay up. Two overflow
// cycles with the camera ready throughout leave two gaps in ready.
func TestCoverageExcludesBacklogSuppression(t *testing.T) {
	logs := captureLogs(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("relay log:\n%s", logs.text())
		}
	})
	r := newRig(t, "")
	r.cfg.Relay.EgressBacklogMB = 1
	r.cfg.Cameras[0].MaxClip = time.Hour
	r.cfg.Cameras[0].TriggerTimeout = time.Hour
	st := newDown(t, r.base)
	run := start(t, r.cfg, st.opts(t))

	r.motion(true)
	eventually(t, "two backlog overflows", 90*time.Second, func() bool {
		return strings.Count(logs.text(), "egress: clip dropped") >= 2
	})
	// Heal the storage so the final flush can publish, and wait for the
	// breaker to close.
	st.down.Store(false)
	eventually(t, "the breaker to close", 30*time.Second, func() bool {
		return strings.Contains(logs.text(), "storage breaker state=closed")
	})
	// The recorder releases the suppressed camera on its next tick.
	time.Sleep(time.Second)
	if _, err := run.stop(t); err != nil {
		t.Fatal(err)
	}

	objs := coverageObjects(t, r.base, "cam-1")
	if len(objs) == 0 {
		t.Fatal("no coverage object was published")
	}
	gaps, drops := 0, 0
	for _, c := range objs {
		gaps += max(0, len(c.Ready)-1)
		drops += len(c.Dropped)
	}
	if drops < 2 {
		t.Errorf("coverage lists %d drops, want at least 2: %+v", drops, objs)
	}
	if gaps < 2 {
		t.Errorf("ready has %d gaps, want one per suppression window (2): the suppressed time is reported as monitored: %+v", gaps, objs)
	}
}
