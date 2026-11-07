package app_test

import (
	"strings"
	"testing"
	"time"
)

// TestRunShutdownWithBlackholedAbortExitsWithinGrace: storage is up while a
// multipart upload starts, then a partition hits the endpoint. The breaker
// opens and the worker parks holding a live ObjectWriter. On SIGTERM that
// upload must be aborted, but the abort goes into the black hole and hangs
// until its own deadline. The process must still exit within
// shutdown_grace_seconds, and the grace left over must still cover the ONVIF
// unsubscribe (relay-spec §4.4, §4.5; review focus 4).
func TestRunShutdownWithBlackholedAbortExitsWithinGrace(t *testing.T) {
	logs := captureLogs(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("relay log:\n%s", logs.text())
		}
	})
	r := newRig(t, "")
	r.cfg.Cameras[0].MaxClip = time.Hour
	r.cfg.Cameras[0].TriggerTimeout = time.Hour
	st := newDown(t, r.base)
	st.down.Store(false)
	run := start(t, r.cfg, st.opts(t))

	r.motion(true)
	eventually(t, "an upload in flight", 60*time.Second, func() bool {
		return len(files(r.base, func(rel string) bool {
			return strings.Contains(rel, "/clips/") && strings.HasSuffix(rel, ".zrec.part")
		})) > 0
	})
	st.blackholeAbort.Store(true)
	st.down.Store(true)
	eventually(t, "the breaker to open", 60*time.Second, func() bool {
		return strings.Contains(logs.text(), "storage breaker state=open")
	})
	time.Sleep(500 * time.Millisecond) // the worker parks with its writer

	took, err := run.stop(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("shutdown with a hanging abort took %v of %v", took, grace)
	if took > grace+graceSlack {
		t.Errorf("Run took %v, want <= shutdown grace %v", took, grace)
	}
	if !strings.Contains(logs.text(), "reason=shutdown") {
		t.Errorf("the dropped clip was not logged with reason shutdown:\n%s", logs.text())
	}
	if strings.Contains(logs.text(), "gave up waiting for an ONVIF unsubscribe") {
		t.Errorf("the abort used up the grace the ONVIF unsubscribe needs:\n%s", logs.text())
	}
}
