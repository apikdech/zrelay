// Package metrics is the relay's Prometheus surface (relay-spec §4.12 plus the
// design §11 additions). It is a leaf package: it imports nothing from the
// relay, and the components it observes reach it through the small hook
// interfaces they define (events.PullMetrics, egress.UploadMetrics) and the
// plain methods below.
//
// Series that exist per camera are created at zero by RegisterCamera, so a
// rate or an alert has a baseline from the first scrape.
package metrics

import (
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// maxUnmatchedSeries bounds the zrelay_events_unmatched_total label set: the
// topic text comes from the camera. Topics beyond the bound are counted under
// OtherTopic.
const maxUnmatchedSeries = 64

// OtherTopic is the topic label of unmatched topics past the cardinality cap.
const OtherTopic = "other"

// endReasons are the clip end reasons of Contract §6.2, so every
// zrelay_clips_total series exists from the start.
var endReasons = []string{
	"post_roll_elapsed", "max_duration", "stream_lost", "codec_change",
	"backlog_overflow", "shutdown", "recovered",
}

// dropReasons are the coverage drop reasons of Contract §7.5.2.
var dropReasons = []string{"stream_lost", "backlog", "shutdown"}

// uploadErrorKinds are the zrelay_upload_errors_total kinds: the storage
// error classes of design §6.3.
var uploadErrorKinds = []string{"transient", "permanent", "not_found"}

// breakerStates names the egress breaker states by value (0 closed,
// 1 half-open, 2 open); the {to} label of the transitions counter.
var breakerStates = [...]string{"closed", "half_open", "open"}

// CameraSources are the values read from live components at scrape time. A
// nil source reads as zero.
type CameraSources struct {
	RingBytes       func() int64  // zrelay_ring_bytes
	RecorderLag     func() uint64 // zrelay_recorder_lag_total (Sub.Lagged of the recorder)
	LiveViewers     func() int    // zrelay_live_viewers
	RestreamClients func() int    // zrelay_restream_clients
}

// Metrics owns every relay series.
type Metrics struct {
	reg      prometheus.Registerer
	gatherer prometheus.Gatherer

	rtspUp, eventsUp *prometheus.GaugeVec
	clips, dropped   *prometheus.CounterVec
	truncated        *prometheus.CounterVec
	gopMax           *prometheus.GaugeVec

	uploadBytes   prometheus.Counter
	uploadErrors  *prometheus.CounterVec
	uploadResumes prometheus.Counter

	resub, retries *prometheus.CounterVec
	skew           *prometheus.GaugeVec
	unmatched      *prometheus.CounterVec

	breakerState prometheus.Gauge
	transitions  *prometheus.CounterVec

	backlog atomic.Pointer[func() int64]

	mu            sync.Mutex
	cameras       map[string]bool
	gopHigh       map[string]float64
	unmatchedSeen map[[2]string]struct{}
}

// New creates every metric and registers it on reg, together with the Go
// runtime and process collectors (the soak samples process_resident_memory_bytes
// and go_goroutines). It panics on a duplicate registration, like
// MustRegister. /metrics gathers from reg when it is also a prometheus.Gatherer
// (a *prometheus.Registry is), otherwise from the default registry.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		reg:           reg,
		cameras:       map[string]bool{},
		gopHigh:       map[string]float64{},
		unmatchedSeen: map[[2]string]struct{}{},
	}
	if g, ok := reg.(prometheus.Gatherer); ok {
		m.gatherer = g
	} else {
		m.gatherer = prometheus.DefaultGatherer
	}
	cam := []string{"camera"}
	m.rtspUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "zrelay_camera_rtsp_up", Help: "1 while the camera's RTSP session is up."}, cam)
	m.eventsUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "zrelay_camera_events_up", Help: "1 while the camera's ONVIF event subscription is healthy."}, cam)
	m.clips = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_clips_total", Help: "Clip segments finished, by end reason."}, []string{"camera", "end_reason"})
	m.dropped = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_clips_dropped_total", Help: "Clips dropped before they were stored, by reason."}, []string{"camera", "reason"})
	m.truncated = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_preroll_truncated_total", Help: "GOPs evicted from the pre-roll ring by its byte cap."}, cam)
	m.gopMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "zrelay_gop_max_seconds", Help: "Longest GOP observed since start."}, cam)
	m.uploadBytes = prometheus.NewCounter(prometheus.CounterOpts{Name: "zrelay_upload_bytes_total", Help: "Ciphertext bytes made durable in storage."})
	m.uploadErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_upload_errors_total", Help: "Failed storage requests of the upload path, by error class."}, []string{"kind"})
	m.uploadResumes = prometheus.NewCounter(prometheus.CounterOpts{Name: "zrelay_upload_resumes_total", Help: "Multipart uploads resumed after a failed write."})
	m.resub = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_onvif_resubscribe_total", Help: "ONVIF subscriptions created again."}, cam)
	m.retries = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_onvif_pull_retries_total", Help: "ONVIF PullMessages transport retries on the same subscription."}, cam)
	m.skew = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "zrelay_clock_skew_seconds", Help: "Camera clock minus relay clock, from the last ONVIF exchange."}, cam)
	m.unmatched = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_events_unmatched_total", Help: "ONVIF notifications matching no rule, by topic."}, []string{"camera", "topic"})
	m.breakerState = prometheus.NewGauge(prometheus.GaugeOpts{Name: "zrelay_storage_breaker_state", Help: "Storage circuit breaker: 0 closed, 1 half-open, 2 open."})
	m.transitions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "zrelay_storage_breaker_transitions_total", Help: "Storage circuit breaker transitions, by target state."}, []string{"to"})

	reg.MustRegister(
		m.rtspUp, m.eventsUp, m.clips, m.dropped, m.truncated, m.gopMax,
		m.uploadBytes, m.uploadErrors, m.uploadResumes,
		m.resub, m.retries, m.skew, m.unmatched,
		m.breakerState, m.transitions,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "zrelay_backlog_bytes", Help: "Ciphertext bytes produced but not yet durable in storage."}, func() float64 {
			if f := m.backlog.Load(); f != nil {
				return float64((*f)())
			}
			return 0
		}),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	for _, name := range breakerStates {
		m.transitions.WithLabelValues(name)
	}
	for _, k := range uploadErrorKinds {
		m.uploadErrors.WithLabelValues(k)
	}
	return m
}

// SetBacklogSource sets where zrelay_backlog_bytes is read from at scrape time.
func (m *Metrics) SetBacklogSource(f func() int64) { m.backlog.Store(&f) }

// RegisterCamera creates the camera's series at zero and registers its
// scrape-time sources. A second call for the same camera does nothing.
func (m *Metrics) RegisterCamera(id string, src CameraSources) {
	m.mu.Lock()
	if m.cameras[id] {
		m.mu.Unlock()
		return
	}
	m.cameras[id] = true
	m.mu.Unlock()

	m.rtspUp.WithLabelValues(id)
	m.eventsUp.WithLabelValues(id)
	m.truncated.WithLabelValues(id)
	m.gopMax.WithLabelValues(id)
	m.resub.WithLabelValues(id)
	m.retries.WithLabelValues(id)
	m.skew.WithLabelValues(id)
	for _, r := range endReasons {
		m.clips.WithLabelValues(id, r)
	}
	for _, r := range dropReasons {
		m.dropped.WithLabelValues(id, r)
	}

	labels := prometheus.Labels{"camera": id}
	m.reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "zrelay_ring_bytes", Help: "Bytes held by the camera's pre-roll ring.", ConstLabels: labels}, func() float64 {
			if src.RingBytes == nil {
				return 0
			}
			return float64(src.RingBytes())
		}),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "zrelay_recorder_lag_total", Help: "Times the recorder's queue overflowed and was resynced.", ConstLabels: labels}, func() float64 {
			if src.RecorderLag == nil {
				return 0
			}
			return float64(src.RecorderLag())
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "zrelay_live_viewers", Help: "Live-view (WebRTC) sessions on the camera.", ConstLabels: labels}, func() float64 {
			if src.LiveViewers == nil {
				return 0
			}
			return float64(src.LiveViewers())
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "zrelay_restream_clients", Help: "RTSP restream clients on the camera.", ConstLabels: labels}, func() float64 {
			if src.RestreamClients == nil {
				return 0
			}
			return float64(src.RestreamClients())
		}),
	)
}

// CameraState records whether the camera's RTSP session and event
// subscription are up.
func (m *Metrics) CameraState(camera string, rtsp, events bool) {
	m.rtspUp.WithLabelValues(camera).Set(b2f(rtsp))
	m.eventsUp.WithLabelValues(camera).Set(b2f(events))
}

// ClipEnded counts a finished clip segment by its end reason.
func (m *Metrics) ClipEnded(camera, endReason string) {
	m.clips.WithLabelValues(camera, endReason).Inc()
}

// ClipDropped counts a dropped clip by its coverage reason.
func (m *Metrics) ClipDropped(camera, reason string) {
	m.dropped.WithLabelValues(camera, reason).Inc()
}

// PreRollTruncated counts a GOP evicted by the ring's byte cap.
func (m *Metrics) PreRollTruncated(camera string) { m.truncated.WithLabelValues(camera).Inc() }

// ObserveGOP raises zrelay_gop_max_seconds when seconds is a new maximum, and
// reports whether it was.
func (m *Metrics) ObserveGOP(camera string, seconds float64) (raised bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if seconds <= m.gopHigh[camera] {
		return false
	}
	m.gopHigh[camera] = seconds
	m.gopMax.WithLabelValues(camera).Set(seconds)
	return true
}

// UploadBytes, UploadError and UploadResume implement egress.UploadMetrics.
func (m *Metrics) UploadBytes(n int64)     { m.uploadBytes.Add(float64(n)) }
func (m *Metrics) UploadError(kind string) { m.uploadErrors.WithLabelValues(kind).Inc() }
func (m *Metrics) UploadResume()           { m.uploadResumes.Inc() }

// BreakerChanged records a breaker transition to state (0 closed, 1 half-open,
// 2 open, the values of egress.BreakerState).
func (m *Metrics) BreakerChanged(state int) {
	if state < 0 || state >= len(breakerStates) {
		return
	}
	m.breakerState.Set(float64(state))
	m.transitions.WithLabelValues(breakerStates[state]).Inc()
}

// Pull is one camera's events.PullMetrics.
type Pull struct {
	m      *Metrics
	camera string
}

// Pull returns the ONVIF hook of camera.
func (m *Metrics) Pull(camera string) *Pull { return &Pull{m: m, camera: camera} }

// PullRetry counts a transport retry on the same subscription.
func (p *Pull) PullRetry() { p.m.retries.WithLabelValues(p.camera).Inc() }

// Resubscribe counts a new subscription.
func (p *Pull) Resubscribe() { p.m.resub.WithLabelValues(p.camera).Inc() }

// ClockSkew records the camera's clock offset.
func (p *Pull) ClockSkew(seconds float64) { p.m.skew.WithLabelValues(p.camera).Set(seconds) }

// Unmatched counts a notification no rule matched. The topic is camera text,
// so the number of distinct series is capped.
func (p *Pull) Unmatched(topic string) {
	key := [2]string{p.camera, topic}
	p.m.mu.Lock()
	if _, seen := p.m.unmatchedSeen[key]; !seen {
		if len(p.m.unmatchedSeen) >= maxUnmatchedSeries {
			topic = OtherTopic
		} else {
			p.m.unmatchedSeen[key] = struct{}{}
		}
	}
	p.m.mu.Unlock()
	p.m.unmatched.WithLabelValues(p.camera, topic).Inc()
}

// Gather returns the current metric families.
func (m *Metrics) Gather() ([]*dto.MetricFamily, error) { return m.gatherer.Gather() }

// Handler serves the Prometheus text exposition.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.gatherer, promhttp.HandlerOpts{})
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
