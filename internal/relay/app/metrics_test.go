package app_test

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
)

// scrape fetches /metrics of the running relay.
func scrape(t *testing.T, listen string) string {
	t.Helper()
	resp, err := http.Get("http://" + listen + "/metrics")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// sample returns the value of the first series of body whose name and labels
// text starts with series (e.g. `zrelay_camera_rtsp_up{camera="cam-1"}`).
func sample(body, series string) (float64, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

// TestRunServesMetrics: /metrics exposes every relay-spec §4.12 series for a
// configured camera, plus the process series the soak samples.
func TestRunServesMetrics(t *testing.T) {
	r := newRig(t, "")
	start(t, r.cfg, app.Options{Version: "test"})

	var body string
	eventually(t, "/metrics", 20*time.Second, func() bool {
		body = scrape(t, r.cfg.Relay.HTTPListen)
		return body != ""
	})
	for _, name := range []string{
		`zrelay_camera_rtsp_up{camera="cam-1"}`,
		`zrelay_camera_events_up{camera="cam-1"}`,
		`zrelay_backlog_bytes`,
		`zrelay_ring_bytes{camera="cam-1"}`,
		`zrelay_gop_max_seconds{camera="cam-1"}`,
		`zrelay_preroll_truncated_total{camera="cam-1"}`,
		`zrelay_upload_bytes_total`,
		`zrelay_upload_resumes_total`,
		`zrelay_onvif_resubscribe_total{camera="cam-1"}`,
		`zrelay_onvif_pull_retries_total{camera="cam-1"}`,
		`zrelay_storage_breaker_state`,
		`zrelay_recorder_lag_total{camera="cam-1"}`,
		`zrelay_live_viewers{camera="cam-1"}`,
		`zrelay_restream_clients{camera="cam-1"}`,
		`zrelay_clips_total{camera="cam-1",end_reason="post_roll_elapsed"}`,
		`zrelay_clips_dropped_total{camera="cam-1",reason="backlog"}`,
		`zrelay_storage_breaker_transitions_total{to="open"}`,
		`process_resident_memory_bytes`,
		`go_goroutines`,
	} {
		if _, ok := sample(body, name); !ok {
			t.Errorf("/metrics lacks series %s", name)
		}
	}
	if v, _ := sample(body, `zrelay_storage_breaker_state`); v != 0 {
		t.Errorf("breaker_state = %v, want 0 (closed)", v)
	}
	// The camera comes up: RTSP and the ONVIF subscription.
	eventually(t, "camera up", 30*time.Second, func() bool {
		b := scrape(t, r.cfg.Relay.HTTPListen)
		rtsp, _ := sample(b, `zrelay_camera_rtsp_up{camera="cam-1"}`)
		ev, _ := sample(b, `zrelay_camera_events_up{camera="cam-1"}`)
		return rtsp == 1 && ev == 1
	})
	// The ring fills, and the fixture's GOP is measured.
	eventually(t, "ring bytes and GOP length", 30*time.Second, func() bool {
		b := scrape(t, r.cfg.Relay.HTTPListen)
		ring, _ := sample(b, `zrelay_ring_bytes{camera="cam-1"}`)
		gop, _ := sample(b, `zrelay_gop_max_seconds{camera="cam-1"}`)
		return ring > 0 && gop >= 1
	})
}

// TestRunCountsClipAndUpload: a recorded clip is counted by its end reason,
// and the uploaded ciphertext shows in upload_bytes_total.
func TestRunCountsClipAndUpload(t *testing.T) {
	r := newRig(t, "")
	start(t, r.cfg, app.Options{Version: "test"})
	eventually(t, "camera up", 30*time.Second, func() bool {
		b := scrape(t, r.cfg.Relay.HTTPListen)
		rtsp, _ := sample(b, `zrelay_camera_rtsp_up{camera="cam-1"}`)
		ev, _ := sample(b, `zrelay_camera_events_up{camera="cam-1"}`)
		return rtsp == 1 && ev == 1
	})

	r.motion(true)
	eventually(t, "a video object being written", 40*time.Second, func() bool {
		return len(files(r.base, func(rel string) bool { return strings.Contains(rel, "/clips/") && strings.Contains(rel, ".zrec") })) > 0
	})
	time.Sleep(3 * time.Second)
	r.motion(false)
	eventually(t, "a clip sidecar", 40*time.Second, func() bool { return len(clipSidecars(r.base)) > 0 })

	eventually(t, "the clip counted", 20*time.Second, func() bool {
		b := scrape(t, r.cfg.Relay.HTTPListen)
		n, _ := sample(b, `zrelay_clips_total{camera="cam-1",end_reason="post_roll_elapsed"}`)
		up, _ := sample(b, `zrelay_upload_bytes_total`)
		return n >= 1 && up > 0
	})
}

// TestRunMetricsShowStorageOutage: with storage down the breaker opens, which
// shows in the state gauge, the transition counter and the upload errors; the
// short outage keeps /healthz healthy (it only fails after five minutes).
func TestRunMetricsShowStorageOutage(t *testing.T) {
	r := newRig(t, "")
	st := newDown(t, r.base)
	start(t, r.cfg, st.opts(t))
	eventually(t, "camera up", 30*time.Second, func() bool {
		b := scrape(t, r.cfg.Relay.HTTPListen)
		rtsp, _ := sample(b, `zrelay_camera_rtsp_up{camera="cam-1"}`)
		ev, _ := sample(b, `zrelay_camera_events_up{camera="cam-1"}`)
		return rtsp == 1 && ev == 1
	})
	r.motion(true)
	eventually(t, "the breaker to open", 60*time.Second, func() bool {
		b := scrape(t, r.cfg.Relay.HTTPListen)
		state, _ := sample(b, `zrelay_storage_breaker_state`)
		n, _ := sample(b, `zrelay_storage_breaker_transitions_total{to="open"}`)
		return n >= 1 && state != 0
	})
	b := scrape(t, r.cfg.Relay.HTTPListen)
	// The test's plain "connection refused" error is a permanent class to the
	// S3 classifier (not a network error type); either kind counts here.
	transient, _ := sample(b, `zrelay_upload_errors_total{kind="transient"}`)
	permanent, _ := sample(b, `zrelay_upload_errors_total{kind="permanent"}`)
	if transient+permanent < 1 {
		t.Errorf("upload_errors_total = %v transient + %v permanent, want >= 1", transient, permanent)
	}
	if backlog, _ := sample(b, `zrelay_backlog_bytes`); backlog <= 0 {
		t.Errorf("backlog_bytes = %v, want > 0 while storage is down", backlog)
	}
	resp, err := http.Get("http://" + r.cfg.Relay.HTTPListen + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d during a short outage, want 200", resp.StatusCode)
	}
}
