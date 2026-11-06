// Package app wires the relay together and runs it (relay-spec §3, design
// §3): storage, circuit breaker, startup recovery, the per-camera pipelines
// (supervisor, recorder and clip machine), live view, restream, egress, the
// status publisher and the HTTP endpoint.
//
// Run starts them in that order and, when its context ends (the SIGTERM
// equivalent), stops them in the order that keeps footage safe: the clip
// machines first, then the egress drain, then the final status flush, then
// the network servers, and last the camera links.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apikdech/zrelay-v2/internal/bearer"
	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/clip"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/egress"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/live"
	"github.com/apikdech/zrelay-v2/internal/relay/recorder"
	"github.com/apikdech/zrelay-v2/internal/relay/recovery"
	"github.com/apikdech/zrelay-v2/internal/relay/restream"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
	"github.com/apikdech/zrelay-v2/internal/relay/status"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/local"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// Options are the parts of Run that tests replace.
type Options struct {
	// Version is reported in sidecars and the status object.
	Version string
	// Clock defaults to clock.Real.
	Clock clock.Clock
	// NewStorage builds the storage driver; it defaults to the driver the
	// config selects (s3 or local).
	NewStorage func(*config.Config) (storage.Provider, storage.Recoverer, error)
}

// recoveryRetryEvery is how often a failed startup recovery is retried while
// the breaker stays closed and nothing else triggers it. Breaker closes retry
// at once (design §6.5); this keeps a quiet relay, which never opens its
// breaker, from staying unrecovered. A variable so tests can shorten it.
var recoveryRetryEvery = time.Minute

const (
	// httpShutdownGrace bounds the HTTP servers' own graceful stop.
	httpShutdownGrace = 2 * time.Second
	// resumeBacklogFraction: a camera suppressed by the backlog policy
	// resumes when the backlog is under this share of the limit (relay-spec
	// §4.5, 50%).
	resumeBacklogFraction = 2
)

// gcPercent is the garbage collector target (GOGC) of the relay: the next
// collection starts when the heap has grown by this share over the live data.
const gcPercent = 20

// s3ResponseTimeout is how long an S3 request may wait for the response
// headers after sending its request.
var s3ResponseTimeout = 30 * time.Second

// s3HTTPClient returns the HTTP client of the s3 driver. The SDK default has
// no response timeout, so a partitioned endpoint would hang a request
// forever and never report the failure the circuit breaker opens on. A
// request body may take as long as the link needs; only the wait for the
// answer, and connection setup, are bounded.
func s3HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: s3ResponseTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}}
}

// DefaultStorage builds the driver cfg selects.
func DefaultStorage(cfg *config.Config) (storage.Provider, storage.Recoverer, error) {
	switch cfg.Storage.Driver {
	case "local":
		d, err := local.New(cfg.Storage.Local.BaseDir, cfg.Storage.Root)
		if err != nil {
			return nil, nil, err
		}
		return d, d, nil
	case "s3":
		s := cfg.Storage.S3
		d, err := s3.New(context.Background(), s3.Config{
			Endpoint:     s.Endpoint,
			Region:       s.Region,
			Bucket:       s.Bucket,
			Root:         cfg.Storage.Root,
			AccessKey:    s.AccessKey,
			SecretKey:    s.SecretKey,
			UsePathStyle: s.UsePathStyle,
			PartSize:     int64(s.PartSizeMiB) << 20,
			HTTPClient:   s3HTTPClient(),
		})
		if err != nil {
			return nil, nil, err
		}
		return d, d, nil
	}
	return nil, nil, fmt.Errorf("unknown storage driver %q", cfg.Storage.Driver)
}

// cam is one camera's pipeline.
type cam struct {
	cfg      config.Camera
	handle   *camera.Camera
	cov      *status.CoverageTracker
	machine  *clip.Machine
	force    chan struct{}
	lastClip atomic.Pointer[time.Time]

	recCancel context.CancelFunc
	recDone   chan struct{}
	evDone    chan struct{} // closed when the event source has unsubscribed
}

// doneSource closes done once its inner source is finished for good: its
// context ended and the source either returned from Run, which is after the
// ONVIF unsubscribe, or was not running at that moment. The second case is
// the supervisor's event loop sleeping in its backoff after the source
// returned early; it exits on the context without calling Run again, so
// waiting for a Run to return would use up the whole shutdown grace.
type doneSource struct {
	events.Source
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	running int
}

// newDoneSource wraps src; ctx is the context the supervisor runs it under.
func newDoneSource(ctx context.Context, src events.Source, done chan struct{}) *doneSource {
	d := &doneSource{Source: src, done: done}
	go func() {
		<-ctx.Done()
		d.mu.Lock()
		idle := d.running == 0
		d.mu.Unlock()
		if idle {
			d.finish()
		}
	}()
	return d
}

func (d *doneSource) finish() { d.once.Do(func() { close(d.done) }) }

func (d *doneSource) Run(ctx context.Context, emit func(events.Signal), ready func(bool)) error {
	d.mu.Lock()
	d.running++
	d.mu.Unlock()
	err := d.Source.Run(ctx, emit, ready)
	d.mu.Lock()
	d.running--
	d.mu.Unlock()
	if ctx.Err() != nil {
		d.finish()
	}
	return err
}

// newEventSource builds the event source of camera c. A variable so a test
// can substitute a source that returns early, which the ONVIF pull point
// never does.
var newEventSource = func(c config.Camera, rules []events.Rule, clk clock.Clock) events.Source {
	return &events.PullPoint{
		Camera:      c.ID,
		Host:        c.Host,
		Port:        c.EventsPort,
		ServicePath: c.Events.ServicePath,
		Username:    c.Username,
		Password:    c.Password,
		Rules:       rules,
		Clock:       clk,
	}
}

// app is the running relay.
type app struct {
	cfg  *config.Config
	opts Options
	clk  clock.Clock

	prov storage.Provider
	rec  storage.Recoverer

	breaker *egress.Breaker
	kick    chan struct{} // the breaker closed: retry recovery

	cams   []*cam
	eg     *egress.Egress
	live   *live.Server
	rs     *restream.Server
	pub    *status.Publisher
	httpSv []*http.Server

	brkCancel, camCancel, egCancel, pubCancel, rcvCancel context.CancelFunc
	brkDone, egDone, pubDone, rcvDone                    chan struct{}
}

// Run runs the relay until ctx is done, then shuts it down within
// shutdown_grace_seconds. A nil error means a clean shutdown; startup
// failures are returned after tearing down whatever had started.
func Run(ctx context.Context, cfg *config.Config, opts Options) error {
	if opts.Clock == nil {
		opts.Clock = clock.Real
	}
	if opts.NewStorage == nil {
		opts.NewStorage = DefaultStorage
	}
	// relay-spec 3.2: the process sets debug.SetMemoryLimit(max_memory_mb).
	// Run restores the previous limit, so tests do not leak it.
	prev := debug.SetMemoryLimit(int64(cfg.Relay.MaxMemoryMB) << 20)
	defer debug.SetMemoryLimit(prev)
	// The memory limit alone lets garbage pile up until the heap nears the
	// limit, and the runtime's total counts every page it ever mapped. The
	// big buffers (the ciphertext rings, the pre-roll rings) hold no
	// pointers, so collecting early is cheap.
	prevGC := debug.SetGCPercent(gcPercent)
	defer debug.SetGCPercent(prevGC)
	a := &app{cfg: cfg, opts: opts, clk: opts.Clock, kick: make(chan struct{}, 1)}
	if err := a.start(); err != nil {
		a.abort()
		return err
	}
	slog.Info("zrelay running", "relay", cfg.Relay.ID, "cameras", len(cfg.Cameras), "version", opts.Version)
	<-ctx.Done()
	slog.Info("zrelay shutting down")
	a.shutdown()
	return nil
}

func (a *app) grace() time.Duration {
	return time.Duration(a.cfg.Relay.ShutdownGraceSeconds) * time.Second
}

// start brings everything up, in order.
func (a *app) start() error {
	cfg := a.cfg

	// Keys: only the derived keys are kept.
	mk, err := zrec.ParseMasterKey(cfg.Encryption.MasterKeyText)
	if err != nil {
		return fmt.Errorf("encryption.master_key: %w", err)
	}
	keys := zrec.Derive(mk)

	// Storage.
	a.prov, a.rec, err = a.opts.NewStorage(cfg)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}

	// The camera records exist from here on; their pipelines start below.
	byID := make(map[string]*cam, len(cfg.Cameras))
	for _, c := range cfg.Cameras {
		cm := &cam{
			cfg:     c,
			cov:     status.NewCoverageTracker(a.clk),
			force:   make(chan struct{}, 1),
			recDone: make(chan struct{}),
			evDone:  make(chan struct{}),
		}
		a.cams = append(a.cams, cm)
		byID[c.ID] = cm
	}
	// Circuit breaker.
	a.breaker = egress.NewBreaker(egress.BreakerConfig{
		FailureThreshold: cfg.Storage.Breaker.FailureThreshold,
		BaseDelay:        cfg.Storage.Breaker.BaseDelay,
		MaxDelay:         cfg.Storage.Breaker.MaxDelay,
	}, a.clk, a.prov.HealthCheck, a.onBreaker)
	var brkCtx context.Context
	brkCtx, a.brkCancel = context.WithCancel(context.Background())
	a.brkDone = make(chan struct{})
	go func() { defer close(a.brkDone); a.breaker.Run(brkCtx) }()

	// Crash recovery, retried on each breaker close until it succeeds once.
	ids := make([]string, len(a.cams))
	for i, cm := range a.cams {
		ids[i] = cm.cfg.ID
	}
	var rcvCtx context.Context
	rcvCtx, a.rcvCancel = context.WithCancel(context.Background())
	a.rcvDone = make(chan struct{})
	go func() { defer close(a.rcvDone); a.recoverLoop(rcvCtx, ids) }()

	backlogLimit := int64(cfg.Relay.EgressBacklogMB) << 20
	a.eg = egress.New(egress.Config{
		Storage:      a.prov,
		Workers:      cfg.Relay.UploadWorkers,
		BacklogLimit: backlogLimit,
		Breaker:      a.breaker,
		Clock:        a.clk,
		Classify:     s3.Classify,
		OnDrop: func(camera, reason string, approxMs int64) {
			if cm := byID[camera]; cm != nil {
				cm.cov.Drop(a.clk.Now(), reason, approxMs)
			}
		},
		OnCommitted: func(camera string, at time.Time) {
			if cm := byID[camera]; cm != nil {
				cm.lastClip.Store(&at)
			}
		},
		ForceFinalize: func(camera string) {
			if cm := byID[camera]; cm != nil {
				select {
				case cm.force <- struct{}{}:
					slog.Warn("backlog full: finalizing the active clip", "camera", camera)
				default: // already asked; the recorder has not acted yet
				}
			}
		},
	})

	// Cameras: supervisor, recorder and clip machine each.
	var camCtx context.Context
	camCtx, a.camCancel = context.WithCancel(context.Background())
	hubs := make(map[string]*camera.Hub, len(a.cams))
	for _, cm := range a.cams {
		if err := a.startCamera(camCtx, cm, keys); err != nil {
			return err
		}
		hubs[cm.cfg.ID] = cm.handle.Hub
	}

	// Live view.
	if cfg.Live.MaxViewers > 0 {
		v, err := bearer.New(cfg.Live.APITokenSHA256)
		if err != nil {
			return fmt.Errorf("live.api_token_sha256: %w", err)
		}
		a.live, err = live.NewServer(live.Config{
			Listen:      cfg.Live.Listen,
			AdvertiseIP: cfg.Live.AdvertiseIP,
			ICEUDPPort:  cfg.Live.ICEUDPPort,
			MaxViewers:  cfg.Live.MaxViewers,
			Verifier:    v,
			Hubs:        hubs,
			ViewerQueue: cfg.Defaults.ViewerQueueFrames,
			Clock:       a.clk,
		})
		if err != nil {
			return fmt.Errorf("live: %w", err)
		}
		if err := a.serve(cfg.Live.Listen, a.live.Handler()); err != nil {
			return fmt.Errorf("live: %w", err)
		}
	}

	// RTSP restream.
	if cfg.Restream.Listen != "" {
		a.rs, err = restream.New(restream.Config{
			Listen:      cfg.Restream.Listen,
			Username:    cfg.Restream.Username,
			Password:    cfg.Restream.Password,
			Hubs:        hubs,
			ViewerQueue: cfg.Defaults.ViewerQueueFrames,
		})
		if err != nil {
			return fmt.Errorf("restream: %w", err)
		}
		if err := a.rs.Start(); err != nil {
			return fmt.Errorf("restream: %w", err)
		}
	}

	// Egress workers.
	var egCtx context.Context
	egCtx, a.egCancel = context.WithCancel(context.Background())
	a.egDone = make(chan struct{})
	go func() { defer close(a.egDone); a.eg.Run(egCtx) }()

	// Status and coverage publisher.
	covs := make(map[string]*status.CoverageTracker, len(a.cams))
	for _, cm := range a.cams {
		covs[cm.cfg.ID] = cm.cov
	}
	a.pub = status.New(status.Config{
		Storage:  a.prov,
		Breaker:  a.breaker,
		RelayID:  cfg.Relay.ID,
		Version:  a.opts.Version,
		KeyID:    keys.ID,
		Cameras:  a.cameraStatus,
		Coverage: covs,
		Backlog:  a.eg.BacklogBytes,
		Clock:    a.clk,
	})
	var pubCtx context.Context
	pubCtx, a.pubCancel = context.WithCancel(context.Background())
	a.pubDone = make(chan struct{})
	go func() { defer close(a.pubDone); a.pub.Run(pubCtx) }()

	// HTTP: /healthz (Task 30 adds /metrics).
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := a.serve(cfg.Relay.HTTPListen, mux); err != nil {
		return fmt.Errorf("http_listen: %w", err)
	}
	return nil
}

// serve listens on addr now, so a bind error fails startup, and serves h in
// the background.
func (a *app) serve(addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	sv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	a.httpSv = append(a.httpSv, sv)
	go func() {
		if err := sv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server stopped", "addr", addr, "err", err)
		}
	}()
	return nil
}

// startCamera wires and starts one camera's pipeline.
func (a *app) startCamera(ctx context.Context, cm *cam, keys zrec.Keys) error {
	c := cm.cfg
	rules, err := events.RulesFor(c.Brand, c.Rules)
	if err != nil {
		return fmt.Errorf("camera %q: %w", c.ID, err)
	}
	src := newDoneSource(ctx, newEventSource(c, rules, a.clk), cm.evDone)
	cm.handle = camera.Supervise(ctx, camera.SuperviseConfig{
		ID:             c.ID,
		RTSPURL:        a.cfg.RTSPURL(c),
		Events:         src,
		TriggerTimeout: c.TriggerTimeout,
		Clock:          a.clk,
		OnState: func(s camera.Status) {
			cm.cov.SetReady(s.State == camera.StateReady, a.clk.Now())
			slog.Info("camera state", "camera", c.ID, "state", string(s.State), "rtsp", s.RTSP, "events", s.Events)
		},
	})

	rec := recorder.New(recorder.Config{
		Camera:    c,
		RelayID:   a.cfg.Relay.ID,
		Version:   a.opts.Version,
		Keys:      keys,
		ChunkSize: a.cfg.Encryption.ChunkSizeKiB << 10,
		Clock:     a.clk,
	}, a.eg)
	b := int64(c.MaxBitrateKbps) * 1000 / 8
	capBytes := b * int64((c.PreRoll+c.MaxGOP)/time.Second) * 5 / 4 // the budget's ring term
	r := ring.New(c.PreRoll, capBytes, func() {
		slog.Warn("pre-roll ring truncated by its byte cap", "camera", c.ID)
	})
	cm.machine = clip.New(clip.Config{Camera: c.ID, PostRoll: c.PostRoll, MaxClip: c.MaxClip}, a.clk, r, rec)

	sub := cm.handle.Hub.Subscribe(camera.ClassRecorder, c.RecorderQueueFrames)
	limit := int64(a.cfg.Relay.EgressBacklogMB) << 20
	resume := func() bool { return a.eg.BacklogBytes() < limit/resumeBacklogFraction }
	var rctx context.Context
	rctx, cm.recCancel = context.WithCancel(context.Background())
	go func() {
		defer close(cm.recDone)
		recorder.Run(rctx, sub, cm.handle.Tracker, cm.machine, recorder.WithBacklogPolicy(cm.force, resume))
	}()
	return nil
}

// cameraStatus is the publisher's camera list.
func (a *app) cameraStatus() []sidecar.CameraStatus {
	out := make([]sidecar.CameraStatus, 0, len(a.cams))
	for _, cm := range a.cams {
		s := cm.handle.Status()
		cs := sidecar.CameraStatus{
			ID: cm.cfg.ID, Name: cm.cfg.Name,
			RTSP: s.RTSP, Events: s.Events,
			Since: sidecar.Time{Time: s.Since.UTC()},
		}
		if t := cm.lastClip.Load(); t != nil {
			cs.LastClipAt = &sidecar.Time{Time: t.UTC()}
		}
		out = append(out, cs)
	}
	return out
}

// onBreaker logs every breaker transition and, on a close, wakes the
// recovery loop. It must not block.
func (a *app) onBreaker(s egress.BreakerState) {
	name := "unknown"
	switch s {
	case egress.Closed:
		name = "closed"
		select {
		case a.kick <- struct{}{}:
		default:
		}
	case egress.HalfOpen:
		name = "half_open"
	case egress.Open:
		name = "open"
	}
	slog.Warn("storage breaker", "state", name)
}

// recoverLoop runs startup crash recovery (design §6.5): at once, then again
// on every breaker close, until it has succeeded once. It only runs while
// the breaker is closed.
func (a *app) recoverLoop(ctx context.Context, ids []string) {
	// Recovery is for the leftovers of a previous process. Every attempt
	// measures candidate ages against the start of this one, so a retry
	// minutes later never takes this process's own in-flight uploads, which
	// are younger than the start, for crashed ones.
	startedAt := a.clk.Now()
	for {
		if st, _ := a.breaker.State(); st == egress.Closed {
			rep, err := recovery.Run(ctx, recovery.Config{
				Provider: a.prov, Recoverer: a.rec, Cameras: ids, Now: startedAt,
			})
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				slog.Info("startup recovery done",
					"recovered", rep.Recovered, "promoted", rep.Promoted, "discarded", rep.Discarded, "orphaned", rep.Orphaned)
				return
			}
			slog.Warn("startup recovery failed; retrying when storage is reachable", "err", err)
		}
		t := a.clk.NewTimer(recoveryRetryEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-a.kick:
		case <-t.C():
		}
		t.Stop()
	}
}

// shutdown stops the relay in order and within the grace period: clip
// machines, egress drain, final status flush, live and restream, then the
// ONVIF subscriptions.
func (a *app) shutdown() {
	deadline := time.Now().Add(a.grace())
	left := func() time.Duration { return max(0, time.Until(deadline)) }

	// 1. Stop triggers: the recorders stop consuming events and their clip
	// machines finalize any open clip with end_reason shutdown.
	for _, cm := range a.cams {
		cm.recCancel()
	}
	for _, cm := range a.cams {
		<-cm.recDone
	}

	// 2. Drain the egress. A reserve of the grace is kept for aborting what
	// cannot finish, the status flush and closing the servers, so the
	// process still exits within shutdown_grace_seconds.
	reserve := min(a.grace()/5, 3*time.Second)
	dropped := a.eg.Shutdown(max(0, left()-reserve))
	if dropped > 0 {
		slog.Warn("shutdown dropped clips that could not be uploaded", "dropped", dropped)
	}
	a.egCancel()
	<-a.egDone

	// 3. Final status and the partial hour of coverage. Flush does nothing
	// while the breaker is not closed.
	a.pubCancel()
	<-a.pubDone
	fctx, fcancel := context.WithTimeout(context.Background(), left())
	a.pub.Flush(fctx)
	fcancel()
	a.rcvCancel()
	a.brkCancel()

	// 4. Live view and restream, then the HTTP endpoints.
	if a.live != nil {
		_ = a.live.Close()
	}
	if a.rs != nil {
		a.rs.Close()
	}
	a.closeHTTP()

	// 5. Camera links; the ONVIF sources unsubscribe on the way out.
	a.camCancel()
	t := time.NewTimer(left())
	defer t.Stop()
wait:
	for _, cm := range a.cams {
		select {
		case <-cm.evDone:
		case <-t.C:
			slog.Warn("shutdown gave up waiting for an ONVIF unsubscribe", "camera", cm.cfg.ID)
			break wait
		}
	}
	<-a.rcvDone
	<-a.brkDone
	_ = a.prov.Close()
}

func (a *app) closeHTTP() {
	for _, sv := range a.httpSv {
		ctx, cancel := context.WithTimeout(context.Background(), httpShutdownGrace)
		if err := sv.Shutdown(ctx); err != nil {
			_ = sv.Close()
		}
		cancel()
	}
	a.httpSv = nil
}

// abort tears down a half-started relay after a startup error. Nothing is
// drained: no clip can have been recorded yet.
func (a *app) abort() {
	for _, cm := range a.cams {
		if cm.recCancel != nil {
			cm.recCancel()
			<-cm.recDone
		}
	}
	if a.egCancel != nil {
		a.egCancel()
		<-a.egDone
	}
	if a.pubCancel != nil {
		a.pubCancel()
		<-a.pubDone
	}
	if a.live != nil {
		_ = a.live.Close()
	}
	if a.rs != nil {
		a.rs.Close()
	}
	a.closeHTTP()
	if a.camCancel != nil {
		a.camCancel()
	}
	if a.rcvCancel != nil {
		a.rcvCancel()
		<-a.rcvDone
	}
	if a.brkCancel != nil {
		a.brkCancel()
		<-a.brkDone
	}
	if a.prov != nil {
		_ = a.prov.Close()
	}
}
