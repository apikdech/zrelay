package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

// discoverConfig returns an example config whose tapo-c220 events port is
// the fake camera's, with the camera environment pointing at the fake.
func discoverConfig(t *testing.T, f *testutil.FakeONVIF) string {
	t.Helper()
	setExampleEnv(t)
	t.Setenv("CAMERA_IP", f.Host)
	t.Setenv("CAMERA_USERNAME", "admin")
	t.Setenv("CAMERA_PASSWORD", "secret")
	return exampleConfigFile(t, func(s string) string {
		out := strings.Replace(s, `events: { source: "onvif", port: 2020 }`,
			`events: { source: "onvif", port: `+strconv.Itoa(f.Port)+` }`, 1)
		if out == s {
			t.Fatal("example config no longer has the events line this test rewrites")
		}
		return out
	})
}

func TestOnvifDiscover(t *testing.T) {
	f := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	f.EventTopic("RuleEngine/CellMotionDetector/Motion", "IsMotion")
	f.Queue("tns1:RuleEngine/CellMotionDetector/Motion", "Initialized", events.Item{Name: "IsMotion", Value: "false"})
	path := discoverConfig(t, f)

	code, out, errOut := runCLI("onvif", "discover", "--config", path, "--camera", "tapo-c220", "--listen", "1500ms")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	for _, want := range []string{"RuleEngine/CellMotionDetector/Motion", "IsMotion", "Suggested rules", "kind: motion"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"secret", "cam-pass"} {
		if strings.Contains(out+errOut, secret) {
			t.Errorf("output contains the camera password %q:\n%s%s", secret, out, errOut)
		}
	}
}

func TestOnvifDiscoverUsageErrors(t *testing.T) {
	f := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	path := discoverConfig(t, f)
	for name, args := range map[string][]string{
		"missing --config":    {"onvif", "discover", "--camera", "tapo-c220"},
		"missing --camera":    {"onvif", "discover", "--config", path},
		"unknown camera":      {"onvif", "discover", "--config", path, "--camera", "nope"},
		"bad --listen":        {"onvif", "discover", "--config", path, "--camera", "tapo-c220", "--listen", "soon"},
		"non-positive listen": {"onvif", "discover", "--config", path, "--camera", "tapo-c220", "--listen", "0s"},
		"missing subcommand":  {"onvif"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, errOut := runCLI(args...); code != 2 || errOut == "" {
				t.Fatalf("exit %d with stderr %q, want 2 with a message", code, errOut)
			}
		})
	}
}
