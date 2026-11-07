package events_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

// followTimers is how many fake-clock timers the client holds while it
// follows a subscription: pull (1 s), Renew (60 s) and clock sync (30 min).
const followTimers = 3

type countingMetrics struct {
	mu          sync.Mutex
	pullRetries int
	resubscribe int
	unmatched   []string
	skew        []float64
}

func (m *countingMetrics) PullRetry()   { m.mu.Lock(); m.pullRetries++; m.mu.Unlock() }
func (m *countingMetrics) Resubscribe() { m.mu.Lock(); m.resubscribe++; m.mu.Unlock() }
func (m *countingMetrics) Unmatched(topic string) {
	m.mu.Lock()
	m.unmatched = append(m.unmatched, topic)
	m.mu.Unlock()
}
func (m *countingMetrics) ClockSkew(s float64) {
	m.mu.Lock()
	m.skew = append(m.skew, s)
	m.mu.Unlock()
}

func (m *countingMetrics) retries() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pullRetries
}

type harness struct {
	t       *testing.T
	fake    *testutil.FakeONVIF
	clk     *clock.Fake
	pp      *events.PullPoint
	metrics *countingMetrics
	tracker *events.Tracker

	mu      sync.Mutex
	signals []events.Signal
	ready   []bool

	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{Username: "admin", Password: "s3cret"})
	clk := clock.NewFake(t0)
	m := &countingMetrics{}
	h := &harness{
		t:       t,
		fake:    fake,
		clk:     clk,
		metrics: m,
		tracker: events.NewTracker(60*time.Second, clk),
		pp: &events.PullPoint{
			Camera:   "front",
			Host:     fake.Host,
			Port:     fake.Port,
			Username: "admin",
			Password: "s3cret",
			Rules:    events.TapoRules(),
			Clock:    clk,
			Metrics:  m,
		},
	}
	return h
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() {
		h.done <- h.pp.Run(ctx, func(s events.Signal) {
			h.mu.Lock()
			h.signals = append(h.signals, s)
			h.mu.Unlock()
			h.tracker.Apply(s)
		}, func(r bool) {
			h.mu.Lock()
			h.ready = append(h.ready, r)
			h.mu.Unlock()
		})
	}()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			h.t.Error("Run did not return after cancel")
		}
	})
}

// stop cancels Run and returns its error.
func (h *harness) stop() error {
	h.t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err // for Cleanup
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("Run did not return after cancel")
		return nil
	}
}

// blockUntil waits until the client holds n fake-clock timers.
func (h *harness) blockUntil(n int) {
	h.t.Helper()
	ok := make(chan struct{})
	go func() { h.clk.BlockUntil(n); close(ok) }()
	select {
	case <-ok:
	case <-time.After(10 * time.Second):
		h.t.Fatalf("timed out waiting for %d pending timers (have %d)", n, h.clk.Pending())
	}
}

// following waits until the client follows a subscription and is idle.
func (h *harness) following() { h.t.Helper(); h.blockUntil(followTimers) }

// pull lets one pull interval pass and waits until it is handled.
func (h *harness) pull() {
	h.t.Helper()
	h.following()
	h.clk.Advance(time.Second)
	h.following()
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; requests: %v", what, ops(h.fake.Requests()))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) gotSignals() []events.Signal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]events.Signal(nil), h.signals...)
}

func (h *harness) gotReady() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.ready...)
}

func ops(reqs []testutil.RecordedRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Op
	}
	return out
}

func requestsFor(reqs []testutil.RecordedRequest, op string) []testutil.RecordedRequest {
	var out []testutil.RecordedRequest
	for _, r := range reqs {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

func lastRequest(t *testing.T, f *testutil.FakeONVIF, op string) testutil.RecordedRequest {
	t.Helper()
	rs := requestsFor(f.Requests(), op)
	if len(rs) == 0 {
		t.Fatalf("no %s request; requests: %v", op, ops(f.Requests()))
	}
	return rs[len(rs)-1]
}

func createdNear(t *testing.T, r testutil.RecordedRequest, want time.Time) {
	t.Helper()
	got, err := time.Parse(time.RFC3339, r.Created)
	if err != nil {
		t.Fatalf("%s Created %q: %v", r.Op, r.Created, err)
	}
	if d := got.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("%s Created = %v, want %v ±2s", r.Op, got, want)
	}
}

func TestPullDeliversSignals(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	h.fake.Queue(motionTopic, "Changed", item("IsMotion", "true"))
	h.fake.Queue(smartTopic, "Changed", item("IsPeople", "true"))
	h.pull()
	if got, want := h.tracker.Active(), []events.Kind{events.KindMotion, events.KindPerson}; !slices.Equal(got, want) {
		t.Fatalf("Active() = %v, want %v; signals %+v", got, want, h.gotSignals())
	}
	if got := h.gotReady(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("ready calls = %v, want [true]", got)
	}
	for _, r := range h.fake.Requests() {
		if r.Op != "GetSystemDateAndTime" && !r.DigestValid {
			t.Fatalf("%s carried an invalid digest", r.Op)
		}
	}
	pull := lastRequest(t, h.fake, "PullMessages")
	if !strings.Contains(pull.Body, "PT1S") || !strings.Contains(pull.Body, "<MessageLimit>20</MessageLimit>") {
		t.Fatalf("PullMessages body lacks Timeout PT1S / MessageLimit 20: %s", pull.Body)
	}
	create := lastRequest(t, h.fake, "CreatePullPointSubscription")
	if create.Path != "/onvif/service" || !strings.Contains(create.Body, "PT120S") {
		t.Fatalf("CreatePullPointSubscription to %s with body %s, want /onvif/service and PT120S", create.Path, create.Body)
	}
}

func TestSignalCarriesTopicInitializedAndCameraAdjustedTime(t *testing.T) {
	h := newHarness(t)
	camera := t0.Add(time.Hour)
	h.fake.CameraTime(camera) // offset +1 h; the message UtcTime is camera time
	h.start()
	h.following()
	h.fake.Queue(motionTopic, "Initialized", item("IsMotion", "true"))
	h.clk.Advance(10 * time.Second)
	h.following()
	sigs := h.gotSignals()
	if len(sigs) != 1 {
		t.Fatalf("signals = %+v, want one", sigs)
	}
	s := sigs[0]
	if s.Camera != "front" || s.Kind != events.KindMotion || !s.Active || !s.Initialized || s.RawTopic != motionTopic {
		t.Fatalf("signal = %+v", s)
	}
	if d := s.At.Sub(t0); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("At = %v, want the message UtcTime in host time (%v) ±2s", s.At, t0)
	}
}

func TestPullTransportErrorRetriesSameSubscription(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	h.fake.DropNextPulls(5)
	for range 5 {
		h.pull()
	}
	if got := h.metrics.retries(); got != 5 {
		t.Fatalf("PullRetry counted %d times, want 5", got)
	}
	if n := h.fake.Count("CreatePullPointSubscription"); n != 1 {
		t.Fatalf("%d CreatePullPointSubscription, want 1 (no resubscribe on transport errors)", n)
	}
	if n := h.fake.Count("Unsubscribe"); n != 0 {
		t.Fatalf("%d Unsubscribe, want 0", n)
	}
	h.fake.Queue(motionTopic, "Changed", item("IsMotion", "true"))
	h.pull()
	if !h.tracker.Triggered() {
		t.Fatal("event after the dropped pulls was not delivered")
	}
	pulls := requestsFor(h.fake.Requests(), "PullMessages")
	if len(pulls) != 6 {
		t.Fatalf("%d pulls, want 6", len(pulls))
	}
	for _, p := range pulls {
		if p.Path != pulls[0].Path {
			t.Fatalf("pull went to %s, then %s: not the same subscription", pulls[0].Path, p.Path)
		}
	}
}

func TestRenewEvery60s(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	h.clk.Advance(59 * time.Second)
	h.following()
	if n := h.fake.Count("Renew"); n != 0 {
		t.Fatalf("%d Renew before 60 s", n)
	}
	h.clk.Advance(time.Second)
	h.following()
	if n := h.fake.Count("Renew"); n != 1 {
		t.Fatalf("%d Renew after 60 s, want 1", n)
	}
	renew := lastRequest(t, h.fake, "Renew")
	if !strings.Contains(renew.Body, "PT120S") || !renew.DigestValid {
		t.Fatalf("Renew body lacks PT120S or digest invalid: %s", renew.Body)
	}
	if renew.Path != lastRequest(t, h.fake, "PullMessages").Path {
		t.Fatalf("Renew went to %s, not the subscription address", renew.Path)
	}
	h.clk.Advance(60 * time.Second)
	h.following()
	if n := h.fake.Count("Renew"); n != 2 {
		t.Fatalf("%d Renew after 120 s, want 2", n)
	}
	if n := h.fake.Count("CreatePullPointSubscription"); n != 1 {
		t.Fatalf("%d subscriptions, want 1", n)
	}
}

func TestFailedRenewResubscribesAfterUnsubscribe(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	h.fake.FailRenew()
	h.clk.Advance(60 * time.Second)
	h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == 1 })
	h.blockUntil(1) // the resubscribe backoff
	h.clk.Advance(5 * time.Second)
	h.following()

	var seq []string
	for _, op := range ops(h.fake.Requests()) {
		if op == "Renew" || op == "Unsubscribe" || op == "CreatePullPointSubscription" {
			seq = append(seq, op)
		}
	}
	want := []string{"CreatePullPointSubscription", "Renew", "Unsubscribe", "CreatePullPointSubscription"}
	if !slices.Equal(seq, want) {
		t.Fatalf("request order = %v, want %v", seq, want)
	}
	unsub := lastRequest(t, h.fake, "Unsubscribe")
	if unsub.Path != lastRequest(t, h.fake, "Renew").Path || !unsub.DigestValid {
		t.Fatalf("Unsubscribe to %s (digest valid %v), want the old subscription address", unsub.Path, unsub.DigestValid)
	}
	h.metrics.mu.Lock()
	defer h.metrics.mu.Unlock()
	if h.metrics.resubscribe != 1 {
		t.Fatalf("Resubscribe counted %d times, want 1", h.metrics.resubscribe)
	}
}

func TestResubscribeBackoff(t *testing.T) {
	h := newHarness(t)
	for range 6 {
		h.fake.Fault("CreatePullPointSubscription")
	}
	h.start()
	for i, d := range []time.Duration{5, 10, 20, 40, 60, 60} {
		d *= time.Second
		h.blockUntil(1)
		h.waitFor("create attempt", func() bool { return h.fake.Count("CreatePullPointSubscription") == i+1 })
		h.clk.Advance(d - time.Millisecond)
		if h.clk.Pending() != 1 {
			t.Fatalf("attempt %d: backoff shorter than %v", i+1, d)
		}
		h.clk.Advance(time.Millisecond)
		h.waitFor("next create attempt", func() bool { return h.fake.Count("CreatePullPointSubscription") == i+2 })
	}
	h.following()
	if got := h.gotReady(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("ready calls = %v, want [true]", got)
	}

	// A successful subscription resets the backoff to 5 s.
	h.fake.FailRenew()
	h.clk.Advance(60 * time.Second)
	h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == 1 })
	h.blockUntil(1)
	h.clk.Advance(5 * time.Second)
	h.following()
	if n := h.fake.Count("CreatePullPointSubscription"); n != 8 {
		t.Fatalf("%d create attempts, want 8", n)
	}
}

func TestNon200PullIsFailure(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	h.fake.Fault("PullMessages")
	h.clk.Advance(time.Second)
	h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == 1 })
	if got := h.metrics.retries(); got != 0 {
		t.Fatalf("a non-200 pull counted as %d transport retries", got)
	}
	h.blockUntil(1)
	h.clk.Advance(5 * time.Second)
	h.following()
	if n := h.fake.Count("CreatePullPointSubscription"); n != 2 {
		t.Fatalf("%d subscriptions, want 2", n)
	}
}

func TestSubscriptionAddressHostRewritten(t *testing.T) {
	h := newHarness(t)
	h.pp.Host = "localhost"
	h.fake.SubscriptionAddress(fmt.Sprintf("http://127.0.0.1:%d/sub", h.fake.SubPort))
	h.start()
	h.pull()
	h.clk.Advance(60 * time.Second) // one Renew
	h.following()
	for _, r := range h.fake.Requests() {
		switch r.Op {
		case "PullMessages", "Renew":
			want := fmt.Sprintf("localhost:%d", h.fake.SubPort)
			if r.Host != want || r.LocalPort != h.fake.SubPort || r.Path != "/sub" {
				t.Fatalf("%s went to host %q port %d path %s, want %q port %d /sub", r.Op, r.Host, r.LocalPort, r.Path, want, h.fake.SubPort)
			}
		default:
			if want := fmt.Sprintf("localhost:%d", h.fake.Port); r.Host != want {
				t.Fatalf("%s went to host %q, want %q", r.Op, r.Host, want)
			}
		}
	}
	if h.fake.Count("Renew") != 1 {
		t.Fatalf("requests %v, want one Renew", ops(h.fake.Requests()))
	}
}

func TestRedirectRefused(t *testing.T) {
	var stolen atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stolen.Add(1)
	}))
	defer other.Close()

	h := newHarness(t)
	h.fake.Redirect("CreatePullPointSubscription", other.URL+"/steal")
	h.start()
	h.waitFor("subscribe attempt", func() bool { return h.fake.Count("CreatePullPointSubscription") == 1 })
	h.blockUntil(1) // backoff: the redirect is a failure
	if n := stolen.Load(); n != 0 {
		t.Fatalf("redirect followed: %d requests reached the other server", n)
	}
	if got := h.gotReady(); len(got) != 0 {
		t.Fatalf("ready calls = %v, want none", got)
	}
}

func TestDigestUsesCameraClockOffset(t *testing.T) {
	h := newHarness(t)
	camera := t0.Add(time.Hour)
	h.fake.CameraTime(camera)
	h.start()
	h.following()
	reqs := h.fake.Requests()
	if len(reqs) < 2 || reqs[0].Op != "GetSystemDateAndTime" {
		t.Fatalf("requests = %v, want GetSystemDateAndTime first", ops(reqs))
	}
	create := lastRequest(t, h.fake, "CreatePullPointSubscription")
	if !create.DigestValid {
		t.Fatal("CreatePullPointSubscription digest invalid")
	}
	createdNear(t, create, camera)

	h.metrics.mu.Lock()
	skew := slices.Clone(h.metrics.skew)
	h.metrics.mu.Unlock()
	if len(skew) != 1 || math.Abs(skew[0]-3600) > 2 {
		t.Fatalf("ClockSkew = %v, want [3600±2]", skew)
	}
}

func TestClockResyncEvery30Minutes(t *testing.T) {
	h := newHarness(t)
	h.fake.CameraTime(t0.Add(time.Hour))
	h.start()
	h.following()
	h.fake.CameraTime(t0.Add(2 * time.Hour)) // the camera clock jumps
	h.clk.Advance(29*time.Minute + 59*time.Second)
	h.following()
	if n := h.fake.Count("GetSystemDateAndTime"); n != 1 {
		t.Fatalf("%d GetSystemDateAndTime before 30 min, want 1", n)
	}
	h.clk.Advance(time.Second)
	h.following()
	if n := h.fake.Count("GetSystemDateAndTime"); n != 2 {
		t.Fatalf("%d GetSystemDateAndTime after 30 min, want 2", n)
	}
	h.pull()
	// New offset: (t0+2h) - (t0+30m) = 1h30m. The pull runs at host t0+30m+1s.
	createdNear(t, lastRequest(t, h.fake, "PullMessages"), t0.Add(2*time.Hour+time.Second))
}

func TestConnectionCloseHeader(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.pull()
	h.clk.Advance(60 * time.Second)
	h.following()
	if err := h.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	reqs := h.fake.Requests()
	seen := map[string]bool{}
	for _, r := range reqs {
		seen[r.Op] = true
		if !r.Close || !strings.EqualFold(r.Header.Get("Connection"), "close") {
			t.Fatalf("%s without Connection: close (Close=%v, header %q)", r.Op, r.Close, r.Header.Get("Connection"))
		}
	}
	for _, op := range []string{"GetSystemDateAndTime", "CreatePullPointSubscription", "PullMessages", "Renew", "Unsubscribe"} {
		if !seen[op] {
			t.Fatalf("no %s request seen; requests %v", op, ops(reqs))
		}
	}
}

func TestUnsubscribeOnShutdown(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	h.fake.Queue(motionTopic, "Changed", item("IsMotion", "true"))
	h.pull()
	if err := h.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if n := h.fake.Count("Unsubscribe"); n != 1 {
		t.Fatalf("%d Unsubscribe on shutdown, want 1", n)
	}
	if h.tracker.Triggered() {
		t.Fatal("kinds still active after shutdown")
	}
	if got := h.gotReady(); !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("ready calls = %v, want [true false]", got)
	}
}

func TestReadyFalseAndKindsResetWhenSubscriptionEnds(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	h.fake.Queue(motionTopic, "Changed", item("IsMotion", "true"))
	h.fake.Queue(smartTopic, "Changed", item("IsPet", "true"))
	h.pull()
	if !h.tracker.Triggered() {
		t.Fatal("not triggered after active events")
	}
	h.fake.FailRenew()
	h.clk.Advance(59 * time.Second)
	h.waitFor("ready(false)", func() bool { return len(h.gotReady()) == 2 })
	if got := h.gotReady(); !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("ready calls = %v, want [true false]", got)
	}
	if h.tracker.Triggered() {
		t.Fatalf("still triggered after the subscription ended: %v", h.tracker.Active())
	}
	// No camera ever said inactive: the client forced every kind inactive.
	forced := map[events.Kind]bool{}
	for _, s := range h.gotSignals() {
		if !s.Active && !s.Initialized {
			forced[s.Kind] = true
		}
	}
	for _, k := range []events.Kind{events.KindMotion, events.KindPerson, events.KindVehicle, events.KindPet, events.KindOther} {
		if !forced[k] {
			t.Fatalf("kind %s not forced inactive; signals %+v", k, h.gotSignals())
		}
	}
}

func TestUnmatchedTopicCounted(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()
	const topic = "tns1:VideoSource/ImageTooDark/ImagingService"
	h.fake.Queue(topic, "Changed", item("State", "true"))
	h.pull()
	h.metrics.mu.Lock()
	unmatched := slices.Clone(h.metrics.unmatched)
	h.metrics.mu.Unlock()
	if !slices.Equal(unmatched, []string{topic}) {
		t.Fatalf("Unmatched = %v, want [%s]", unmatched, topic)
	}
	if sigs := h.gotSignals(); len(sigs) != 0 {
		t.Fatalf("unmatched topic emitted %+v", sigs)
	}
}

func TestResponseCappedAt1MiB(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.following()

	// Under the cap: parsed.
	h.fake.Queue(motionTopic, "Changed", item("IsMotion", "true"))
	h.fake.PadNextPull(900 << 10)
	h.pull()
	if !h.tracker.Triggered() {
		t.Fatal("a 900 KiB reply was not parsed")
	}

	// Over the cap: rejected without being read whole, and a failure.
	h.fake.Queue(motionTopic, "Changed", item("IsMotion", "false"))
	h.fake.PadNextPull(1 << 20)
	h.clk.Advance(time.Second)
	h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == 1 })
	for _, s := range h.gotSignals() {
		if s.RawTopic == motionTopic && !s.Active {
			t.Fatalf("signal %+v parsed from a reply over 1 MiB", s)
		}
	}
	if got := h.metrics.retries(); got != 0 {
		t.Fatalf("oversized reply counted as %d transport retries", got)
	}
}

// A camera that accepts CreatePullPointSubscription but rejects every
// PullMessages must not be resubscribed every 6 s: the backoff keeps growing
// 5, 10, 20, 40, 60, 60 s, because a subscription that never pulled
// successfully is not healthy (design §4.3).
func TestResubscribeBackoffGrowsWhenCreateWorksButPullFails(t *testing.T) {
	h := newHarness(t)
	for range 6 {
		h.fake.Fault("PullMessages")
	}
	h.start()
	for i, d := range []time.Duration{5, 10, 20, 40, 60, 60} {
		d *= time.Second
		h.following()
		h.clk.Advance(time.Second) // the pull that fails
		h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == i+1 })
		h.blockUntil(1) // the backoff sleep
		h.clk.Advance(d - time.Millisecond)
		if h.clk.Pending() != 1 {
			t.Fatalf("cycle %d: backoff shorter than %v", i+1, d)
		}
		h.clk.Advance(time.Millisecond)
		h.waitFor("next create attempt", func() bool { return h.fake.Count("CreatePullPointSubscription") == i+2 })
	}
}

// newDiscoveryHarness is a harness whose fake camera serves its event
// service only at /onvif/event_service, reachable by GetCapabilities or
// GetServices on the device service.
func newDiscoveryHarness(t *testing.T, opts testutil.FakeONVIFOpts) *harness {
	t.Helper()
	opts.Username, opts.Password = "admin", "s3cret"
	opts.EventPath = "/onvif/event_service"
	h := newHarness(t)
	h.fake = testutil.StartFakeONVIF(t, opts)
	h.pp.Host, h.pp.Port = h.fake.Host, h.fake.Port
	return h
}

// checkDiscovered fails unless the event service was found at the camera's
// advertised path on the configured host and port, never on the host the
// camera named, and was not looked up again.
func checkDiscovered(t *testing.T, h *harness, lookup string) {
	t.Helper()
	h.following()
	if n := h.fake.Count("CreatePullPointSubscription"); n < 2 {
		t.Fatalf("%d CreatePullPointSubscription requests, want a failure at the default path and a success", n)
	}
	last := lastRequest(t, h.fake, "CreatePullPointSubscription")
	if last.Path != "/onvif/event_service" || last.Host != fmt.Sprintf("%s:%d", h.fake.Host, h.fake.Port) {
		t.Fatalf("subscribed at %s%s, want the configured host with /onvif/event_service", last.Host, last.Path)
	}
	for _, r := range h.fake.Requests() {
		if strings.Contains(r.Host, "192.0.2.77") {
			t.Fatalf("%s sent to the camera-named host %s", r.Op, r.Host)
		}
	}
	if n := h.fake.Count(lookup); n != 1 {
		t.Fatalf("%d %s requests, want 1", n, lookup)
	}
	if got := h.gotReady(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("ready calls = %v, want [true]", got)
	}
}

// With events.service_path unset and the default path failing, the event
// service is found with GetCapabilities (design §4.3).
func TestEndpointDiscoveredByGetCapabilities(t *testing.T) {
	h := newDiscoveryHarness(t, testutil.FakeONVIFOpts{})
	h.start()
	checkDiscovered(t, h, "GetCapabilities")
	dev := lastRequest(t, h.fake, "GetCapabilities")
	if dev.Path != "/onvif/device_service" || !strings.Contains(dev.Body, "Events") {
		t.Fatalf("GetCapabilities to %s with body %q, want the device service asking for Events", dev.Path, dev.Body)
	}
}

// GetServices is the fallback when GetCapabilities is refused.
func TestEndpointDiscoveredByGetServicesFallback(t *testing.T) {
	h := newDiscoveryHarness(t, testutil.FakeONVIFOpts{NoGetCapabilities: true})
	h.start()
	checkDiscovered(t, h, "GetServices")
}

// The discovered endpoint is cached for the session: a resubscribe goes
// straight to it.
func TestDiscoveredEndpointCachedAcrossResubscribe(t *testing.T) {
	h := newDiscoveryHarness(t, testutil.FakeONVIFOpts{})
	h.start()
	checkDiscovered(t, h, "GetCapabilities")
	creates := h.fake.Count("CreatePullPointSubscription")
	h.fake.FailRenew()
	h.clk.Advance(60 * time.Second)
	h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == 1 })
	h.blockUntil(1)
	h.clk.Advance(5 * time.Second)
	h.following()
	if n := h.fake.Count("CreatePullPointSubscription") - creates; n != 1 {
		t.Fatalf("%d creates after resubscribe, want 1 (no default-path retry)", n)
	}
	if n := h.fake.Count("GetCapabilities"); n != 1 {
		t.Fatalf("%d GetCapabilities requests, want 1", n)
	}
}

// An explicit service_path is never second-guessed, and a working default
// needs no discovery.
func TestNoDiscoveryWhenPathConfiguredOrDefaultWorks(t *testing.T) {
	h := newDiscoveryHarness(t, testutil.FakeONVIFOpts{})
	h.pp.ServicePath = "/onvif/wrong"
	h.start()
	h.blockUntil(1)
	h.waitFor("create attempt", func() bool { return h.fake.Count("CreatePullPointSubscription") == 1 })
	if n := h.fake.Count("GetCapabilities") + h.fake.Count("GetServices"); n != 0 {
		t.Fatalf("%d discovery requests with service_path set", n)
	}

	h2 := newHarness(t)
	h2.start()
	h2.following()
	if n := h2.fake.Count("GetCapabilities") + h2.fake.Count("GetServices"); n != 0 {
		t.Fatalf("%d discovery requests with a working default path", n)
	}
}

// A subscription that has pulled successfully is healthy: when it later
// fails, the retry is after 5 s again.
func TestBackoffResetsAfterHealthySubscription(t *testing.T) {
	h := newHarness(t)
	for range 3 {
		h.fake.Fault("PullMessages")
	}
	h.start()
	for i, d := range []time.Duration{5, 10, 20} {
		h.following()
		h.clk.Advance(time.Second)
		h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == i+1 })
		h.blockUntil(1)
		h.clk.Advance(d * time.Second)
	}
	h.following()
	h.pull() // a good pull
	h.fake.FailRenew()
	h.clk.Advance(60 * time.Second)
	h.waitFor("Unsubscribe", func() bool { return h.fake.Count("Unsubscribe") == 4 })
	h.blockUntil(1)
	h.clk.Advance(5 * time.Second)
	h.following()
	if n := h.fake.Count("CreatePullPointSubscription"); n != 5 {
		t.Fatalf("%d create attempts, want 5 (backoff reset after a healthy subscription)", n)
	}
}
