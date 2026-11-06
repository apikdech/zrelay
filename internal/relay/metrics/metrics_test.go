package metrics_test

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/metrics"
)

// wantNames is every metric of relay-spec §4.12 plus the design §11 additions,
// as a literal list: renaming a metric must fail this test.
var wantNames = []string{
	"zrelay_camera_rtsp_up",
	"zrelay_camera_events_up",
	"zrelay_clips_total",
	"zrelay_clips_dropped_total",
	"zrelay_backlog_bytes",
	"zrelay_ring_bytes",
	"zrelay_gop_max_seconds",
	"zrelay_preroll_truncated_total",
	"zrelay_upload_bytes_total",
	"zrelay_upload_errors_total",
	"zrelay_upload_resumes_total",
	"zrelay_onvif_resubscribe_total",
	"zrelay_onvif_pull_retries_total",
	"zrelay_clock_skew_seconds",
	"zrelay_events_unmatched_total",
	"zrelay_storage_breaker_state",
	"zrelay_storage_breaker_transitions_total",
	"zrelay_recorder_lag_total",
	"zrelay_live_viewers",
	"zrelay_restream_clients",
	// The soak (design §8.3, runbook) reads these two.
	"process_resident_memory_bytes",
	"go_goroutines",
}

func gather(t *testing.T, m *metrics.Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	fams, err := m.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range fams {
		out[f.GetName()] = f
	}
	return out
}

// value returns the value of the sample of family name whose labels include
// all of want, for a gauge or a counter.
func value(t *testing.T, m *metrics.Metrics, name string, want map[string]string) float64 {
	t.Helper()
	f := gather(t, m)[name]
	if f == nil {
		t.Fatalf("metric %s missing", name)
	}
next:
	for _, s := range f.Metric {
		got := map[string]string{}
		for _, l := range s.Label {
			got[l.GetName()] = l.GetValue()
		}
		for k, v := range want {
			if got[k] != v {
				continue next
			}
		}
		if s.Counter != nil {
			return s.Counter.GetValue()
		}
		return s.Gauge.GetValue()
	}
	t.Fatalf("metric %s has no sample with labels %v", name, want)
	return 0
}

func newWithCamera(t *testing.T) *metrics.Metrics {
	t.Helper()
	m := metrics.New(prometheus.NewRegistry())
	m.RegisterCamera("cam1", metrics.CameraSources{})
	return m
}

func expect(t *testing.T, m *metrics.Metrics, name string, labels map[string]string, want float64) {
	t.Helper()
	if got := value(t, m, name, labels); got != want {
		t.Errorf("%s%v = %v, want %v", name, labels, got, want)
	}
}

var cam1 = map[string]string{"camera": "cam1"}

func TestAllMetricsRegistered(t *testing.T) {
	m := newWithCamera(t)
	// Series with an open-ended label exist only once observed.
	m.Pull("cam1").Unmatched("tns1:Some/Topic")

	fams := gather(t, m)
	for _, name := range wantNames {
		if fams[name] == nil {
			t.Errorf("metric %s is not exposed", name)
		}
	}
}

func TestNewRegistersOnceOnTheGivenRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.New(reg)
	defer func() {
		if recover() == nil {
			t.Fatal("a second New on the same registry should panic (duplicate registration)")
		}
	}()
	metrics.New(reg)
}

func TestCameraStateGauges(t *testing.T) {
	m := newWithCamera(t)
	m.CameraState("cam1", true, false)
	expect(t, m, "zrelay_camera_rtsp_up", cam1, 1)
	expect(t, m, "zrelay_camera_events_up", cam1, 0)
}

func TestClipCounters(t *testing.T) {
	m := newWithCamera(t)
	m.ClipEnded("cam1", "post_roll_elapsed")
	m.ClipEnded("cam1", "post_roll_elapsed")
	m.ClipDropped("cam1", "backlog")
	expect(t, m, "zrelay_clips_total", map[string]string{"camera": "cam1", "end_reason": "post_roll_elapsed"}, 2)
	expect(t, m, "zrelay_clips_dropped_total", map[string]string{"camera": "cam1", "reason": "backlog"}, 1)
}

func TestUploadCounters(t *testing.T) {
	m := newWithCamera(t)
	m.UploadBytes(100)
	m.UploadBytes(23)
	m.UploadError("transient")
	m.UploadResume()
	expect(t, m, "zrelay_upload_bytes_total", nil, 123)
	expect(t, m, "zrelay_upload_errors_total", map[string]string{"kind": "transient"}, 1)
	expect(t, m, "zrelay_upload_resumes_total", nil, 1)
}

func TestBreakerGaugeAndTransitions(t *testing.T) {
	m := newWithCamera(t)
	m.BreakerChanged(2)
	m.BreakerChanged(1)
	m.BreakerChanged(0)
	m.BreakerChanged(2)
	expect(t, m, "zrelay_storage_breaker_state", nil, 2)
	expect(t, m, "zrelay_storage_breaker_transitions_total", map[string]string{"to": "open"}, 2)
	expect(t, m, "zrelay_storage_breaker_transitions_total", map[string]string{"to": "half_open"}, 1)
	expect(t, m, "zrelay_storage_breaker_transitions_total", map[string]string{"to": "closed"}, 1)
}

func TestBacklogAndCameraSourcesAreReadAtScrapeTime(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	backlog, ringBytes := int64(5), int64(7)
	viewers, clients := 2, 3
	lag := uint64(4)
	m.SetBacklogSource(func() int64 { return backlog })
	m.RegisterCamera("cam1", metrics.CameraSources{
		RingBytes:       func() int64 { return ringBytes },
		RecorderLag:     func() uint64 { return lag },
		LiveViewers:     func() int { return viewers },
		RestreamClients: func() int { return clients },
	})
	expect(t, m, "zrelay_backlog_bytes", nil, 5)
	expect(t, m, "zrelay_ring_bytes", cam1, 7)
	expect(t, m, "zrelay_recorder_lag_total", cam1, 4)
	expect(t, m, "zrelay_live_viewers", cam1, 2)
	expect(t, m, "zrelay_restream_clients", cam1, 3)

	backlog, ringBytes, viewers, clients, lag = 50, 70, 1, 0, 9
	expect(t, m, "zrelay_backlog_bytes", nil, 50)
	expect(t, m, "zrelay_ring_bytes", cam1, 70)
	expect(t, m, "zrelay_recorder_lag_total", cam1, 9)
	expect(t, m, "zrelay_live_viewers", cam1, 1)
	expect(t, m, "zrelay_restream_clients", cam1, 0)
}

func TestGOPMaxKeepsTheHighWaterMark(t *testing.T) {
	m := newWithCamera(t)
	m.ObserveGOP("cam1", 2)
	m.ObserveGOP("cam1", 5.5)
	m.ObserveGOP("cam1", 3)
	expect(t, m, "zrelay_gop_max_seconds", cam1, 5.5)
}

func TestPreRollTruncated(t *testing.T) {
	m := newWithCamera(t)
	m.PreRollTruncated("cam1")
	expect(t, m, "zrelay_preroll_truncated_total", cam1, 1)
}

// The ONVIF adapter must satisfy the events package's hook interface.
var _ events.PullMetrics = (*metrics.Pull)(nil)

func TestPullMetricsAdapter(t *testing.T) {
	m := newWithCamera(t)
	p := m.Pull("cam1")
	p.PullRetry()
	p.PullRetry()
	p.Resubscribe()
	p.Unmatched("tns1:A/B")
	p.Unmatched("tns1:A/B")
	p.ClockSkew(-3.5)
	expect(t, m, "zrelay_onvif_pull_retries_total", cam1, 2)
	expect(t, m, "zrelay_onvif_resubscribe_total", cam1, 1)
	expect(t, m, "zrelay_events_unmatched_total", map[string]string{"camera": "cam1", "topic": "tns1:A/B"}, 2)
	expect(t, m, "zrelay_clock_skew_seconds", cam1, -3.5)
}

// Topics come from the camera, so the label set must not grow without bound.
func TestUnmatchedTopicCardinalityIsBounded(t *testing.T) {
	m := newWithCamera(t)
	p := m.Pull("cam1")
	for i := range 1000 {
		p.Unmatched(fmt.Sprintf("tns1:Topic/%d", i))
	}
	n := len(gather(t, m)["zrelay_events_unmatched_total"].Metric)
	if n > 65 {
		t.Errorf("%d distinct unmatched-topic series; want a bounded number (<= 65)", n)
	}
	// Once the cap is reached, counts still land somewhere visible.
	var total float64
	for _, s := range gather(t, m)["zrelay_events_unmatched_total"].Metric {
		total += s.Counter.GetValue()
	}
	if total != 1000 {
		t.Errorf("total unmatched = %v, want 1000", total)
	}
}
