package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const h264Fixture = "../../../../testdata/video/h264-640x360-gop2-10s.h264"

// probeConfig writes a config for one camera on the local fakes.
func probeConfig(t *testing.T, rtspURL string, onvifPort int) string {
	t.Helper()
	// live and restream default ON (design §5); the plain probe turns both off.
	return probeConfigWith(t, rtspURL, onvifPort, "live: { max_viewers: 0 }\nrestream: { listen: \"\" }\n")
}

// probeConfigWith is probeConfig plus extra top-level YAML (live, restream).
func probeConfigWith(t *testing.T, rtspURL string, onvifPort int, extra string) string {
	t.Helper()
	u, err := url.Parse(rtspURL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	yml := fmt.Sprintf(`version: 2
allow_literal_secrets: true
relay:
  id: "probe"
  max_memory_mb: 768
  egress_backlog_mb: 64
encryption:
  master_key: "zk1-literal-for-tests"
storage:
  driver: "local"
  root: "zrelay"
  local:
    base_dir: %q
cameras:
  - id: "cam-1"
    brand: "tapo"
    host: "127.0.0.1"
    credentials: { username: "admin", password: "secret" }
    rtsp: { port: %s, path: "/stream" }
    events: { source: "onvif", port: %d }
%s`, filepath.Join(dir, "archive"), u.Port(), onvifPort, extra)
	p := filepath.Join(dir, "probe.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCamprobeLogsStatesRatesAndResources runs the probe against the test
// RTSP server and fake ONVIF camera. ZRELAY_CAMPROBE_SMOKE=2m (a duration)
// makes it the two-minute smoke run of docs/LIVE-CHECKS.md; run it with -v
// to see the log.
func TestCamprobeLogsStatesRatesAndResources(t *testing.T) {
	duration, interval := 3*time.Second, 500*time.Millisecond
	if v := os.Getenv("ZRELAY_CAMPROBE_SMOKE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("ZRELAY_CAMPROBE_SMOKE: %v", err)
		}
		duration, interval = d, 10*time.Second
	}
	rtsp := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: testutil.LoadAnnexB(t, h264Fixture), Loop: true})
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	cfg := probeConfig(t, rtsp.URL(), onvif.Port)

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--config", cfg, "--camera", "cam-1", "--interval", interval.String(), "--duration", duration.String(),
	}, &out, &errOut)
	t.Logf("camprobe output:\n%s", out.String())
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut.String())
	}

	text := out.String()
	if !strings.Contains(text, "state=ready") {
		t.Errorf("the probe never logged state=ready:\n%s", text)
	}
	stats := regexp.MustCompile(`(?m)^.*au_per_s=(\d+\.\d+) goroutines=(\d+) rss_kib=(\d+)$`).FindAllStringSubmatch(text, -1)
	if len(stats) < 2 {
		t.Fatalf("got %d periodic stats lines, want at least 2:\n%s", len(stats), text)
	}
	last := stats[len(stats)-1]
	if last[1] == "0.0" {
		t.Errorf("last stats line has au_per_s=0.0: %v", last[0])
	}
	if last[3] == "0" {
		t.Errorf("last stats line has no RSS: %v", last[0])
	}
	if strings.Contains(text, "secret") {
		t.Errorf("the probe printed the camera password:\n%s", text)
	}
}

func TestCamprobeUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"missing --config":  {"--camera", "cam-1"},
		"missing --camera":  {"--config", "x.yaml"},
		"non-positive tick": {"--config", "x.yaml", "--camera", "c", "--interval", "0s"},
		"unreadable config": {"--config", "/nonexistent/zrelay.yaml", "--camera", "c"},
		"negative duration": {"--config", "x.yaml", "--camera", "c", "--duration", "-1s"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(context.Background(), args, &out, &errOut); code != 2 || errOut.Len() == 0 {
				t.Fatalf("exit %d with stderr %q, want 2 with a message", code, errOut.String())
			}
		})
	}
}
