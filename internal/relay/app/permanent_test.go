package app_test

import (
	"strings"
	"testing"
	"time"
)

// TestPermanentStorageFailureIsLoggedAtErrorWithItsCause: a rotated secret
// key or a denying policy answers 403 while HeadBucket may still succeed, so
// the breaker flaps. The operator must be able to tell that from an outage:
// design §6.3 logs a permanent failure at error level once per transition,
// with the (redacted) error.
func TestPermanentStorageFailureIsLoggedAtErrorWithItsCause(t *testing.T) {
	logs := captureLogs(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("relay log:\n%s", logs.text())
		}
	})
	r := newRig(t, "")
	st := newDown(t, r.base)
	st.down.Store(false)
	st.denied.Store(true)
	start(t, r.cfg, st.opts(t))

	r.motion(true)
	eventually(t, "a permanent storage failure to be logged", 60*time.Second, func() bool {
		return strings.Contains(logs.errorText(), "AccessDenied")
	})
	if !strings.Contains(logs.errorText(), "permanent") {
		t.Errorf("the error log does not say the failure is permanent:\n%s", logs.errorText())
	}
	for _, secret := range []string{"secret", "zk1-"} {
		if strings.Contains(logs.text(), secret) {
			t.Errorf("log leaks %q:\n%s", secret, logs.text())
		}
	}
}
