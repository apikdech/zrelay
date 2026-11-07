package app_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/testutil"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/local"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const h264Fixture = "../../../testdata/video/h264-640x360-gop2-10s.h264"

// grace is shutdown_grace_seconds of the test configs.
const grace = 3 * time.Second

// graceSlack is the scheduling tolerance on the grace bound: Run is measured
// from this goroutine's cancel to the receive of its result.
const graceSlack = 50 * time.Millisecond

// rig is one fake camera (RTSP and ONVIF) and a config pointing at it.
type rig struct {
	rtsp  *testutil.RTSPServer
	onvif *testutil.FakeONVIF
	base  string // local driver base_dir
	cfg   *config.Config
}

// freePort returns a TCP port that was free a moment ago. The tests that
// need it cannot learn the port from the app, which binds http_listen itself.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func newRig(t *testing.T, extra string) *rig {
	t.Helper()
	rtsp := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: testutil.LoadAnnexB(t, h264Fixture), Loop: true})
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	return newRigFor(t, rtsp, onvif, extra)
}

func newRigFor(t *testing.T, rtsp *testutil.RTSPServer, onvif *testutil.FakeONVIF, extra string) *rig {
	t.Helper()
	u, err := url.Parse(rtsp.URL())
	if err != nil {
		t.Fatal(err)
	}
	port := u.Port()
	dir := t.TempDir()
	base := filepath.Join(dir, "archive")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	mk, err := zrec.GenerateMasterKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	yml := fmt.Sprintf(`version: 2
allow_literal_secrets: true
relay:
  id: "test-relay"
  max_memory_mb: 768
  egress_backlog_mb: 64
  shutdown_grace_seconds: %d
  http_listen: %q
encryption:
  master_key: %q
storage:
  driver: "local"
  root: "zrelay"
  local:
    base_dir: %q
  breaker:
    failure_threshold: 2
    base_delay: "100ms"
    max_delay: "200ms"
defaults:
  pre_roll_seconds: 2
  post_roll_seconds: 1
  max_clip_seconds: 60
  trigger_timeout_seconds: 120
# live and restream default ON (design §5); these rigs run without them.
live: { max_viewers: 0 }
restream: { listen: "" }
cameras:
  - id: "cam-1"
    name: "Camera One"
    brand: "tapo"
    host: "127.0.0.1"
    credentials: { username: "admin", password: "secret" }
    rtsp: { port: %s, path: "/stream" }
    events: { source: "onvif", port: %d }
%s`, int(grace/time.Second), freePort(t), mk.String(), base, port, onvif.Port, extra)
	p := filepath.Join(dir, "zrelay.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return &rig{rtsp: rtsp, onvif: onvif, base: base, cfg: cfg}
}

const motionTopic = "tns1:RuleEngine/CellMotionDetector/Motion"

func (r *rig) motion(active bool) {
	r.onvif.Queue(motionTopic, "Changed", events.Item{Name: "IsMotion", Value: fmt.Sprint(active)})
}

// running is an app.Run in a goroutine.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, cfg *config.Config, opts app.Options) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- app.Run(ctx, cfg, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(30 * time.Second):
			t.Error("app.Run did not return after cancel")
		}
	})
	return r
}

// stop cancels the context (the SIGTERM equivalent) and returns how long Run
// took to return.
func (r *running) stop(t *testing.T) (time.Duration, error) {
	t.Helper()
	t0 := time.Now()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		return time.Since(t0), err
	case <-time.After(grace + 20*time.Second):
		t.Fatal("app.Run did not return after cancel")
		return 0, nil
	}
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// files returns the paths under dir whose slash-separated relative path
// satisfies match.
func files(dir string, match func(rel string) bool) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if match(filepath.ToSlash(rel)) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func clipSidecars(base string) []string {
	return files(base, func(rel string) bool {
		name := filepath.Base(rel)
		return strings.Contains(rel, "/clips/") && strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".")
	})
}

func TestRunRecordsClipToLocal(t *testing.T) {
	r := newRig(t, "")
	run := start(t, r.cfg, app.Options{Version: "test"})

	// Wait for a ready camera: the first status heartbeat reports it.
	statusFile := filepath.Join(r.base, "zrelay", "status", "relay.json")
	eventually(t, "status/relay.json", 30*time.Second, func() bool {
		_, err := os.Stat(statusFile)
		return err == nil
	})

	// Motion starts a clip once the camera is ready (the notification stays
	// active until it is cleared); a few seconds in, clear it and the clip
	// ends after the 1 s post-roll.
	r.motion(true)
	eventually(t, "a video object being written", 40*time.Second, func() bool {
		return len(files(r.base, func(rel string) bool { return strings.Contains(rel, "/clips/") && strings.Contains(rel, ".zrec") })) > 0
	})
	time.Sleep(3 * time.Second)
	r.motion(false)
	eventually(t, "a clip sidecar", 40*time.Second, func() bool { return len(clipSidecars(r.base)) > 0 })

	took, err := run.stop(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took > grace {
		t.Errorf("Run took %v to stop, want <= shutdown grace %v", took, grace)
	}

	for _, p := range clipSidecars(r.base) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var sc sidecar.Sidecar
		if err := json.Unmarshal(b, &sc); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if sc.Camera.ID != "cam-1" || sc.Recorder.RelayID != "test-relay" || sc.Recorder.Version != "test" {
			t.Errorf("sidecar identity = %+v / %+v", sc.Camera, sc.Recorder)
		}
		video := filepath.Join(filepath.Dir(p), sc.Video.Object)
		if fi, err := os.Stat(video); err != nil || fi.Size() != sc.Video.SizeBytes {
			t.Errorf("video %s: %v, size %v want %d", video, err, fi, sc.Video.SizeBytes)
		}
	}
	// Shutdown flushed the partial hour of coverage.
	cov := files(r.base, func(rel string) bool { return strings.Contains(rel, "status/coverage/") })
	if len(cov) == 0 {
		t.Error("no coverage object after shutdown")
	}
	// No staging sidecar is left behind by a clean run.
	if left := files(r.base, func(rel string) bool { return strings.HasSuffix(rel, ".json.part") }); len(left) != 0 {
		t.Errorf("staging sidecars left: %v", left)
	}
}

// downStorage is a provider whose writes and probes fail while down.
type downStorage struct {
	storage.Provider
	rec storage.Recoverer

	down atomic.Bool
	// recoveryFails makes the recovery scan fail while writes still work.
	recoveryFails atomic.Bool

	mu          sync.Mutex
	healthCalls int
	listCalls   int
	listOK      int
}

var errDown = errors.New("connection refused")

func newDown(t *testing.T, base string) *downStorage {
	t.Helper()
	d, err := local.New(base, "zrelay")
	if err != nil {
		t.Fatal(err)
	}
	s := &downStorage{Provider: d, rec: d}
	s.down.Store(true)
	return s
}

func (s *downStorage) Create(ctx context.Context, key string) (storage.ObjectWriter, error) {
	if s.down.Load() {
		return nil, errDown
	}
	return s.Provider.Create(ctx, key)
}

func (s *downStorage) PutSmall(ctx context.Context, key string, data []byte, ct string) error {
	if s.down.Load() {
		return errDown
	}
	return s.Provider.PutSmall(ctx, key, data, ct)
}

func (s *downStorage) Delete(ctx context.Context, key string) error {
	if s.down.Load() {
		return errDown
	}
	return s.Provider.Delete(ctx, key)
}

func (s *downStorage) HealthCheck(ctx context.Context) error {
	s.mu.Lock()
	s.healthCalls++
	s.mu.Unlock()
	if s.down.Load() {
		return errDown
	}
	return s.Provider.HealthCheck(ctx)
}

func (s *downStorage) ListIncomplete(ctx context.Context, prefix string, older time.Duration) ([]storage.Incomplete, error) {
	if s.down.Load() {
		return nil, errDown
	}
	out, err := s.rec.ListIncomplete(ctx, prefix, older)
	if err == nil {
		s.mu.Lock()
		s.listOK++
		s.mu.Unlock()
	}
	return out, err
}

func (s *downStorage) RecoverCommit(ctx context.Context, inc storage.Incomplete) (storage.ObjectInfo, error) {
	return s.rec.RecoverCommit(ctx, inc)
}

// ListStaging is the first call of every recovery scan, so it counts the
// attempts; a scan that gets past it and ListIncomplete counts as a success.
func (s *downStorage) ListStaging(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	s.mu.Lock()
	s.listCalls++
	s.mu.Unlock()
	if s.down.Load() || s.recoveryFails.Load() {
		return nil, errDown
	}
	return s.rec.ListStaging(ctx, prefix)
}

func (s *downStorage) counts() (health, list, ok int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthCalls, s.listCalls, s.listOK
}

func (s *downStorage) opts(t *testing.T) app.Options {
	return app.Options{
		Version: "test",
		NewStorage: func(*config.Config) (storage.Provider, storage.Recoverer, error) {
			return s, s, nil
		},
	}
}

// logCapture collects slog records for the test.
type logCapture struct {
	mu   sync.Mutex
	recs []string
}

func (l *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	l.mu.Lock()
	l.recs = append(l.recs, b.String())
	l.mu.Unlock()
	return nil
}
func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logCapture) WithGroup(string) slog.Handler      { return l }
func (l *logCapture) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.recs, "\n")
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	l := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}

// TestRunShutdownWithStorageDownExitsWithinGrace is relay-spec §4.8 / the
// plan's review focus 4 at the wiring level: with storage unreachable and a
// finalized clip in the backlog, SIGTERM still ends the process within
// shutdown_grace_seconds, and the clip is logged as dropped (shutdown).
func TestRunShutdownWithStorageDownExitsWithinGrace(t *testing.T) {
	logs := captureLogs(t)
	r := newRig(t, "")
	st := newDown(t, r.base)
	run := start(t, r.cfg, st.opts(t))

	r.motion(true)
	eventually(t, "the breaker to open", 60*time.Second, func() bool {
		return strings.Contains(logs.text(), "storage breaker state=open")
	})
	// A clip is recording; it ends with the shutdown.
	time.Sleep(time.Second)

	took, err := run.stop(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The spec bound is shutdown_grace_seconds itself; the slack covers only
	// the scheduling of this test's own goroutines.
	if took > grace+graceSlack {
		t.Errorf("Run took %v with storage down, want <= shutdown grace %v", took, grace)
	}
	t.Logf("shutdown with storage down took %v of %v", took, grace)
	if !strings.Contains(logs.text(), "reason=shutdown") {
		t.Errorf("the dropped clip was not logged with reason shutdown:\n%s", logs.text())
	}
	// The grace the drain and the flush leave over still covers the ONVIF
	// unsubscribe: the camera is told to drop its subscription.
	if strings.Contains(logs.text(), "gave up waiting for an ONVIF unsubscribe") {
		t.Errorf("shutdown left no grace for the ONVIF unsubscribe:\n%s", logs.text())
	}
	for _, line := range strings.Split(logs.text(), "\n") {
		if strings.Contains(line, "secret") || strings.Contains(line, "zk1-") {
			t.Errorf("log line leaks a credential: %s", line)
		}
	}
}

// TestRunRetriesRecoveryOnBreakerClose: recovery that fails at startup is
// retried each time the breaker closes, until it succeeds once (design §6.5).
func TestRunRetriesRecoveryOnBreakerClose(t *testing.T) {
	logs := captureLogs(t)
	r := newRig(t, "")
	st := newDown(t, r.base)
	start(t, r.cfg, st.opts(t))

	eventually(t, "the startup recovery attempt", 20*time.Second, func() bool {
		_, list, _ := st.counts()
		return list >= 1
	})
	if _, _, ok := st.counts(); ok != 0 {
		t.Fatalf("recovery succeeded while storage was down")
	}

	r.motion(true)
	eventually(t, "the breaker to open", 60*time.Second, func() bool {
		return strings.Contains(logs.text(), "storage breaker state=open")
	})
	st.down.Store(false)
	eventually(t, "recovery to run again after the breaker closed", 60*time.Second, func() bool {
		_, _, ok := st.counts()
		return ok >= 1
	})
	if !strings.Contains(logs.text(), "storage breaker state=closed") {
		t.Errorf("recovery ran without the breaker closing:\n%s", logs.text())
	}
	// Once successful, recovery is not repeated on later closes.
	_, _, ok := st.counts()
	time.Sleep(time.Second)
	if _, _, later := st.counts(); later != ok {
		t.Errorf("recovery repeated after it had succeeded: %d -> %d", ok, later)
	}
}

func TestRunFailsWhenStorageCannotBeBuilt(t *testing.T) {
	r := newRig(t, "")
	err := app.Run(context.Background(), r.cfg, app.Options{
		NewStorage: func(*config.Config) (storage.Provider, storage.Recoverer, error) {
			return nil, nil, errors.New("boom")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run error = %v, want one naming boom", err)
	}
}

// TestRunServesHealthzAndRestream wires http_listen and the restream.
func TestRunServesHealthzAndRestream(t *testing.T) {
	restream := freePort(t)
	r := newRig(t, "")
	// Restream is set on the loaded config: the rig's YAML ends with the
	// cameras list.
	r.cfg.Restream = config.Restream{Listen: restream, Username: "viewer", Password: "pw"}
	start(t, r.cfg, app.Options{Version: "test"})

	url := "http://" + r.cfg.Relay.HTTPListen + "/healthz"
	eventually(t, "/healthz", 20*time.Second, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	eventually(t, "the restream port", 20*time.Second, func() bool {
		c, err := net.Dial("tcp", restream)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
}

// TestRunSetsMemoryLimit: relay-spec 3.2, the process sets
// debug.SetMemoryLimit(max_memory_mb) at startup, so the collector works
// harder before the process outgrows its budget. Run restores the previous
// limit when it returns.
func TestRunSetsMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	r := newRig(t, "")
	run := start(t, r.cfg, app.Options{Version: "test"})
	eventually(t, "the memory limit to be set", 10*time.Second, func() bool {
		return debug.SetMemoryLimit(-1) == int64(r.cfg.Relay.MaxMemoryMB)<<20
	})
	if _, err := run.stop(t); err != nil {
		t.Fatal(err)
	}
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Errorf("memory limit after Run = %d, want the previous %d", got, prev)
	}
}

// TestRunServesLiveWithBearerAuth wires live.max_viewers > 0: the WHEP server
// listens on live.listen, rejects a request without the K_api bearer (401),
// answers a valid bearer for an unknown camera with 404, and stops with Run.
func TestRunServesLiveWithBearerAuth(t *testing.T) {
	r := newRig(t, "")
	mk, err := zrec.ParseMasterKey(r.cfg.Encryption.MasterKeyText)
	if err != nil {
		t.Fatal(err)
	}
	keys := zrec.Derive(mk)
	hash := zrec.APITokenHash(keys)
	listen := freePort(t)
	r.cfg.Live = config.Live{
		Listen:         listen,
		AdvertiseIP:    "127.0.0.1",
		MaxViewers:     1,
		APITokenSHA256: []string{hex.EncodeToString(hash[:])},
	}
	run := start(t, r.cfg, app.Options{Version: "test"})

	post := func(path, bearer string) (int, error) {
		req, err := http.NewRequest(http.MethodPost, "http://"+listen+path, strings.NewReader("v=0\r\n"))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/sdp")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}

	var noAuth int
	eventually(t, "the live port", 20*time.Second, func() bool {
		code, err := post("/live/cam-1", "")
		noAuth = code
		return err == nil
	})
	if noAuth != http.StatusUnauthorized {
		t.Errorf("POST /live/cam-1 without a bearer = %d, want 401", noAuth)
	}
	if code, err := post("/live/cam-1", "not-the-token"); err != nil || code != http.StatusUnauthorized {
		t.Errorf("POST /live/cam-1 with a wrong bearer = %d, %v, want 401", code, err)
	}
	if code, err := post("/live/unknown", zrec.BearerToken(keys)); err != nil || code != http.StatusNotFound {
		t.Errorf("POST /live/unknown with a valid bearer = %d, %v, want 404", code, err)
	}

	if _, err := run.stop(t); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", listen, time.Second); err == nil {
		c.Close()
		t.Errorf("live port %s still accepts connections after Run returned", listen)
	}
}

// shiftClock is the real clock running ahead by an offset the test sets: wall
// time jumps, timers still run in real time.
type shiftClock struct {
	clock.Clock
	off atomic.Int64
}

func (c *shiftClock) Now() time.Time          { return c.Clock.Now().Add(time.Duration(c.off.Load())) }
func (c *shiftClock) advance(d time.Duration) { c.off.Add(int64(d)) }

// TestRecoveryRetryNeverTouchesThisProcessesClips: recovery handles the
// leftovers of a previous process, so its reference time is the moment this
// process started. A retry that used the current time would, after more than
// two minutes, take this process's own in-flight clip for a crashed one,
// commit its upload under the live writer and delete its staging sidecar.
func TestRecoveryRetryNeverTouchesThisProcessesClips(t *testing.T) {
	logs := captureLogs(t)
	t.Cleanup(app.SetRecoveryRetryEvery(100 * time.Millisecond))
	r := newRig(t, "")
	// The jump of the clock must not end the clip by itself.
	r.cfg.Cameras[0].TriggerTimeout = time.Hour
	r.cfg.Cameras[0].MaxClip = time.Hour
	st := newDown(t, r.base)
	st.down.Store(false)
	st.recoveryFails.Store(true)
	clk := &shiftClock{Clock: clock.Real}
	o := st.opts(t)
	o.Clock = clk
	start(t, r.cfg, o)

	eventually(t, "the startup recovery attempt", 20*time.Second, func() bool {
		_, list, _ := st.counts()
		return list >= 1
	})
	r.motion(true)
	inFlight := func(suffix string) []string {
		return files(r.base, func(rel string) bool { return strings.Contains(rel, "/clips/") && strings.HasSuffix(rel, suffix) })
	}
	eventually(t, "an upload in flight with its staging sidecar", 60*time.Second, func() bool {
		return len(inFlight(".zrec.part")) > 0 && len(inFlight(".json.part")) > 0
	})

	// More than two minutes pass, then the retry gets through.
	clk.advance(5 * time.Minute)
	st.recoveryFails.Store(false)
	eventually(t, "the recovery retry to succeed", 20*time.Second, func() bool {
		return strings.Contains(logs.text(), "startup recovery done")
	})

	if len(inFlight(".zrec.part")) == 0 {
		t.Error("the in-flight upload was committed by recovery")
	}
	if len(inFlight(".json.part")) == 0 {
		t.Error("the in-flight clip's staging sidecar was deleted by recovery")
	}
	if got := clipSidecars(r.base); len(got) != 0 {
		t.Errorf("recovery published a sidecar for a clip still being written: %v", got)
	}
	if !strings.Contains(logs.text(), "recovered=0 promoted=0 discarded=0 orphaned=0") {
		t.Errorf("recovery touched something:\n%s", logs.text())
	}
}

// TestRunShutdownWithONVIFDownIsPrompt: review focus 5, the camera's event
// endpoint refuses connections while RTSP works. Shutdown must not wait out
// the grace period for an ONVIF unsubscribe that has nothing to unsubscribe.
// The real pull point keeps running (it retries inside Run) until its context
// ends, so this does not exercise the supervisor's own backoff; that is
// TestRunShutdownWithEventSourceInBackoffIsPrompt.
func TestRunShutdownWithONVIFDownIsPrompt(t *testing.T) {
	logs := captureLogs(t)
	r := newRig(t, "")
	_, port, err := net.SplitHostPort(freePort(t)) // nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.Cameras[0].EventsPort = p
	run := start(t, r.cfg, app.Options{Version: "test"})

	eventually(t, "the events link to be reported down", 30*time.Second, func() bool {
		return strings.Contains(logs.text(), "onvif events interrupted")
	})
	took, err := run.stop(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took > grace/2 {
		t.Errorf("Run took %v to stop with ONVIF down, want well under the %v grace", took, grace)
	}
	if strings.Contains(logs.text(), "gave up waiting for an ONVIF unsubscribe") {
		t.Errorf("shutdown waited out the grace for the ONVIF unsubscribe:\n%s", logs.text())
	}
}

// earlySource is an event source whose Run returns at once with an error, the
// way a source with a bug or a future source type may: the supervisor's event
// loop then sleeps in its backoff and calls Run again only when it ends.
type earlySource struct{ runs atomic.Int32 }

func (s *earlySource) Run(context.Context, func(events.Signal), func(bool)) error {
	s.runs.Add(1)
	return errors.New("source stopped early")
}

// TestRunShutdownWithEventSourceInBackoffIsPrompt is the scenario of review
// round 1: the supervisor's event loop is asleep in its backoff (the source
// returned early and is not running) when SIGTERM arrives. The loop exits on
// its context without calling Run again, so shutdown must not wait for a Run
// to return and use up the grace period. The real PullPoint only returns when
// its context ends, so this needs a source that returns early; with the
// idle-finish branch of doneSource disabled this test fails with a wait of
// the whole grace and the "gave up waiting" log line.
func TestRunShutdownWithEventSourceInBackoffIsPrompt(t *testing.T) {
	logs := captureLogs(t)
	src := &earlySource{}
	t.Cleanup(app.SetEventSource(func(config.Camera, []events.Rule, clock.Clock) events.Source { return src }))
	r := newRig(t, "")
	run := start(t, r.cfg, app.Options{Version: "test"})

	// Two stops: the backoff after the second is at most 2 s and after the
	// third at most 4 s, so the loop is asleep for nearly all the time after
	// the second stop.
	eventually(t, "the supervisor to back off from the stopped source", 30*time.Second, func() bool {
		return strings.Count(logs.text(), "event source stopped") >= 2
	})
	time.Sleep(20 * time.Millisecond) // the log precedes the sleep by microseconds
	took, err := run.stop(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took > grace/2 {
		t.Errorf("Run took %v to stop with the event source in backoff, want well under the %v grace", took, grace)
	}
	if strings.Contains(logs.text(), "gave up waiting for an ONVIF unsubscribe") {
		t.Errorf("shutdown waited out the grace for an unsubscribe that was not running:\n%s", logs.text())
	}
}

// TestRunForceFinalizesThroughTheBacklogPolicy drives the egress backlog
// policy through the wiring, in CI: with storage down and a 1 MiB backlog,
// the accountant asks to force-finalize the recording camera (egress
// ForceFinalize -> the app -> the recorder's WithBacklogPolicy -> the clip
// machine). The finalized clip is then evicted, the backlog empties, the
// recorder releases the suppressed camera (resume), a new clip starts, and
// the backlog policy finalizes that one too.
//
// Max clip and trigger timeout are an hour, so nothing but the policy can end
// a clip inside the test window. The evidence is therefore the drop of a
// finalized clip: the ForceFinalize callback's own log line is printed on
// every accountant call whether or not the signal reaches the recorder, so it
// proves nothing about the wiring. Two drops need the whole cycle: finalize,
// suppress, resume, record again, finalize again.
func TestRunForceFinalizesThroughTheBacklogPolicy(t *testing.T) {
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
	start(t, r.cfg, st.opts(t))

	r.motion(true)
	// The fixture is about 70 kB/s: a clip fills 1 MiB in ~15 s.
	eventually(t, "the backlog policy to finalize a clip, resume the camera and finalize another",
		90*time.Second, func() bool {
			return strings.Count(logs.text(), "egress: clip dropped") >= 2
		})
	if got := strings.Count(logs.text(), "reason=backlog"); got < 2 {
		t.Errorf("the finalized clips were not dropped by the backlog policy (%d drops):\n%s", got, logs.text())
	}
}

// TestRunTightensTheGCPercent: with only the memory limit, the collector
// lets the heap grow with garbage up to the limit, and the runtime's total
// memory (which also counts pages already returned to the OS) ends above
// max_memory_mb. The ring buffers are pointer-free, so a low GC percent costs
// little; Run sets it and restores the previous value when it returns.
func TestRunTightensTheGCPercent(t *testing.T) {
	read := func() int {
		p := debug.SetGCPercent(100)
		debug.SetGCPercent(p)
		return p
	}
	prev := read()
	r := newRig(t, "")
	run := start(t, r.cfg, app.Options{Version: "test"})
	eventually(t, "the GC percent to be set", 10*time.Second, func() bool { return read() == app.GCPercent })
	if _, err := run.stop(t); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != prev {
		t.Errorf("GC percent after Run = %d, want the previous %d", got, prev)
	}
}
