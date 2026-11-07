package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/recorder"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

const (
	// DropShutdown is the drop reason for clips abandoned at shutdown
	// (relay-spec §4.8; coverage cannot be published while storage is down).
	DropShutdown = "shutdown"

	// abortTimeout bounds the Abort and staging cleanup of a dropped clip, in
	// real time: it runs when storage may be unreachable.
	abortTimeout = 5 * time.Second
	// safetySlack is how long past the abort budget Shutdown waits for the
	// workers, whose requests end on the same deadline, to report back.
	safetySlack = 50 * time.Millisecond
	// deleteStagingTries bounds the outer retries of the staging delete.
	deleteStagingTries = 3
)

// ErrShuttingDown is returned by Begin once Shutdown was called.
var ErrShuttingDown = errors.New("egress: shutting down")

// Config configures an Egress.
type Config struct {
	Storage      storage.Writer
	Workers      int   // concurrent drain workers (upload_workers); <= 0 means 1
	BacklogLimit int64 // bytes (egress_backlog_mb)
	Breaker      *Breaker
	Clock        clock.Clock
	// Classify maps a storage error to its retry class (s3.Classify).
	Classify func(error) storage.ErrorClass
	// OnDrop reports a dropped clip: its camera, the reason ("backlog",
	// "shutdown") and approximately how many ms of footage it held.
	OnDrop func(camera, reason string, approxMs int64)
	// OnCommitted reports a clip that became visible (video committed and
	// final sidecar stored).
	OnCommitted func(camera string, at time.Time)
	// ForceFinalize asks the camera's recorder to finalize its current clip.
	ForceFinalize func(camera string)
	// Metrics (may be nil) observes the upload path.
	Metrics UploadMetrics
}

// UploadMetrics observes the upload path (zrelay_upload_* metrics). The
// implementation must not block.
type UploadMetrics interface {
	// UploadBytes reports n more ciphertext bytes made durable in storage.
	UploadBytes(n int64)
	// UploadError reports a failed storage request by error class:
	// "transient", "permanent" or "not_found".
	UploadError(kind string)
	// UploadResume reports a multipart upload resumed after a failed write.
	UploadResume()
}

// Egress is the memory-only upload path: it implements recorder.ClipSink,
// buffering each clip's ciphertext in a Stream and draining it to storage
// with a bounded pool of workers (design §6.3, §6.4).
type Egress struct {
	cfg  Config
	acct *Accountant

	// hctx is cancelled to hard-stop every worker: by Shutdown after the grace
	// period, or when Run's context ends.
	hctx    context.Context
	hcancel context.CancelFunc

	// abortBy is the real-time deadline (UnixNano) of the shutdown abort
	// phase, or 0 while no shutdown is past its drain. Aborts started after
	// that point end by it instead of after abortTimeout.
	abortBy atomic.Int64

	mu    sync.Mutex
	queue []*job
	jobs  map[*Clip]*job
	shut  bool
	wake  chan struct{} // capacity 1: the queue is not empty
}

var _ recorder.ClipSink = (*Egress)(nil)

// job is one clip travelling through the drain workers.
type job struct {
	clip    *Clip
	pc      *recorder.PendingClip
	claimed atomic.Bool // a worker (or Shutdown) took the job
	// committed is set once the video object is committed; from then on the
	// clip is visible after recovery and is no longer dropped by Shutdown.
	committed atomic.Bool
	// pendingSince is the clock time (UnixNano) at which the storage request
	// now in flight started, or 0 when none is.
	pendingSince atomic.Int64
	finishing    sync.Once
	done         chan struct{}
}

// New returns an Egress. Call Run to start its workers.
func New(cfg Config) *Egress {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real
	}
	if cfg.Classify == nil {
		cfg.Classify = func(error) storage.ErrorClass { return storage.Transient }
	}
	// A driver that reports a vanished upload through storage.ErrUploadGone
	// (the s3 driver's Commit does, with a Permanent class) was answered by
	// storage, so it is NotFound here: Do reports the breaker a success, not a
	// permanent failure, and persist ends the attempt with errUploadGone.
	classify := cfg.Classify
	cfg.Classify = func(err error) storage.ErrorClass {
		if errors.Is(err, storage.ErrUploadGone) {
			return storage.NotFound
		}
		return classify(err)
	}
	e := &Egress{
		cfg:  cfg,
		jobs: make(map[*Clip]*job),
		wake: make(chan struct{}, 1),
	}
	e.hctx, e.hcancel = context.WithCancel(context.Background())
	e.acct = NewAccountant(cfg.BacklogLimit, e.reportDrop, cfg.ForceFinalize)
	return e
}

func (e *Egress) uploadBytes(n int64) {
	if e.cfg.Metrics != nil && n > 0 {
		e.cfg.Metrics.UploadBytes(n)
	}
}

// uploadError reports err, a failed storage request, with its class.
func (e *Egress) uploadError(err error) {
	if e.cfg.Metrics == nil {
		return
	}
	switch e.cfg.Classify(err) {
	case storage.Permanent:
		e.cfg.Metrics.UploadError("permanent")
	case storage.NotFound:
		e.cfg.Metrics.UploadError("not_found")
	default:
		e.cfg.Metrics.UploadError("transient")
	}
}

// BacklogBytes returns the ciphertext bytes produced but not yet durable.
func (e *Egress) BacklogBytes() int64 { return e.acct.Total() }

func (e *Egress) approxMs(c *Clip) int64 {
	return max(0, e.cfg.Clock.Now().Sub(c.Created).Milliseconds())
}

func (e *Egress) reportDrop(c *Clip, reason string) {
	if c.Committed.Load() {
		// The video was committed before the drop was recorded: recovery
		// promotes it, so coverage must not report a gap.
		slog.Warn("egress: eviction raced a commit; clip kept", "camera", c.Camera, "key", c.Keys.Video, "reason", reason)
		return
	}
	slog.Warn("egress: clip dropped", "camera", c.Camera, "key", c.Keys.Video, "reason", reason)
	if e.cfg.OnDrop != nil {
		e.cfg.OnDrop(c.Camera, reason, e.approxMs(c))
	}
}

// drop evicts j's stream and records the drop unless something already did.
// It reports whether this call dropped the clip.
func (e *Egress) drop(j *job, reason string) bool {
	if !j.clip.Stream.Evict() {
		return false
	}
	e.reportDrop(j.clip, reason)
	return true
}

// cameraOf extracts the camera id from a clip directory
// ("clips/<camera>/YYYY/MM/DD").
func cameraOf(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 {
		return parts[1]
	}
	return dir
}

// Begin registers a new clip and returns the writer for its ciphertext. The
// writer never blocks. The staging sidecar, the multipart upload and the
// commit happen on a drain worker, in that order (design §6.4).
func (e *Egress) Begin(pc *recorder.PendingClip) (io.Writer, error) {
	s := NewStream()
	c := &Clip{
		Camera:  cameraOf(pc.Keys.Dir),
		Keys:    pc.Keys,
		Staging: pc.Staging,
		Final:   pc.Final,
		Stream:  s,
		Created: e.cfg.Clock.Now(),
	}
	j := &job{clip: c, pc: pc, done: make(chan struct{})}

	e.mu.Lock()
	if e.shut {
		e.mu.Unlock()
		return nil, ErrShuttingDown
	}
	e.jobs[c] = j
	e.queue = append(e.queue, j)
	e.mu.Unlock()
	e.acct.Register(c)

	pc.Done = func(err error) {
		if err != nil {
			// The recorder discarded the clip: the worker aborts the
			// upload. This is not a drop.
			s.Evict()
			return
		}
		c.Finalized.Store(true)
		s.CloseWrite()
		// A force-finalized clip gets no further Write to trigger the policy.
		e.acct.Enforce()
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return s, nil
}

// next pops the oldest queued job, or returns nil when ctx ends.
func (e *Egress) next(ctx context.Context) *job {
	for {
		e.mu.Lock()
		if len(e.queue) > 0 {
			j := e.queue[0]
			e.queue[0] = nil
			e.queue = e.queue[1:]
			if len(e.queue) > 0 {
				select {
				case e.wake <- struct{}{}:
				default:
				}
			}
			e.mu.Unlock()
			return j
		}
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-e.wake:
		}
	}
}

// Run starts the drain workers and blocks until ctx ends and they have
// stopped. Cancelling ctx hard-stops running uploads (they are aborted and
// counted as dropped with reason "shutdown"); for a graceful stop call
// Shutdown first.
func (e *Egress) Run(ctx context.Context) {
	stop := context.AfterFunc(ctx, e.hcancel)
	defer stop()
	var wg sync.WaitGroup
	for range e.cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j := e.next(ctx)
				if j == nil {
					return
				}
				e.runJob(j)
			}
		}()
	}
	wg.Wait()
}

// Shutdown stops accepting clips and waits up to grace for every queued clip
// to finish. Clips still unfinished then are aborted by their owning workers
// and recorded as dropped with reason "shutdown"; it returns how many. Run
// should still be running while Shutdown executes. The abort phase may take
// up to abortTimeout; see ShutdownWithin to bound it.
func (e *Egress) Shutdown(grace time.Duration) (dropped int) {
	return e.ShutdownWithin(grace, abortTimeout)
}

// ShutdownWithin is Shutdown with a budget for the abort phase that follows
// the drain: every abort started after the grace ends by abort (at most
// abortTimeout) from the end of the grace, and Shutdown returns by then even
// if a worker is still hung in a request into an unreachable endpoint. A
// caller that must exit within shutdown_grace_seconds reserves abort for it.
func (e *Egress) ShutdownWithin(grace, abort time.Duration) (dropped int) {
	abort = min(abort, abortTimeout)
	e.mu.Lock()
	e.shut = true
	pending := make([]*job, 0, len(e.jobs))
	for _, j := range e.jobs {
		pending = append(pending, j)
	}
	e.mu.Unlock()

	allDone := make(chan struct{})
	go func() {
		for _, j := range pending {
			<-j.done
		}
		close(allDone)
	}()
	timer := e.cfg.Clock.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-allDone:
		return 0
	case <-timer.C():
	}

	// Grace is over. The abort phase has a real-time budget (storage is
	// probably unreachable). Evict first (that is what makes the owning
	// worker abort and is what counts the drop), then cancel in-flight
	// requests.
	abortEnd := time.Now().Add(abort)
	e.abortBy.Store(abortEnd.UnixNano())
	for _, j := range pending {
		select {
		case <-j.done:
			continue
		default:
		}
		if j.committed.Load() {
			continue // visible after recovery; only its sidecar is pending
		}
		if e.drop(j, DropShutdown) {
			dropped++
		}
	}
	e.hcancel()
	// Jobs no worker has started have nothing to abort.
	for _, j := range pending {
		if j.claimed.CompareAndSwap(false, true) {
			e.finish(j)
		}
	}
	// Workers abort with bounded contexts; do not wait for a hung one forever.
	safety := time.NewTimer(time.Until(abortEnd) + safetySlack)
	defer safety.Stop()
	for _, j := range pending {
		select {
		case <-j.done:
		case <-safety.C:
			slog.Warn("egress: shutdown gave up waiting for a drain worker")
			return dropped
		}
	}
	return dropped
}

// InFlight reports whether key is the video or staging sidecar key of a clip
// this Egress is still writing. Startup recovery uses it to leave the
// process's own uploads alone.
func (e *Egress) InFlight(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for c := range e.jobs {
		if key == c.Keys.Video || key == c.Keys.StagingSidecar {
			return true
		}
	}
	return false
}

// abortContext returns the bounded fresh context of an abort or cleanup
// request. During the shutdown abort phase it ends by that phase's deadline.
func (e *Egress) abortContext() (context.Context, context.CancelFunc) {
	ctx := context.WithoutCancel(e.hctx)
	if by := e.abortBy.Load(); by != 0 {
		return context.WithDeadline(ctx, time.Unix(0, by))
	}
	return context.WithTimeout(ctx, abortTimeout)
}

// finish releases j's accounting and wakes waiters. It is idempotent.
func (e *Egress) finish(j *job) {
	j.finishing.Do(func() {
		e.acct.Unregister(j.clip)
		e.mu.Lock()
		delete(e.jobs, j.clip)
		e.mu.Unlock()
		close(j.done)
	})
}
