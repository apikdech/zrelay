package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// copyBufSize is the read size of the drain loop. Each worker owns one buffer,
// so the memory outside the rings is Workers x copyBufSize.
const copyBufSize = 256 << 10

// runJob drains one clip to storage (design §6.4): staging sidecar, multipart
// upload, commit, final sidecar, staging delete. It runs on a worker
// goroutine, which is also the only goroutine that ever calls Abort on the
// clip's ObjectWriter (relay-spec §4.8: a single owner).
func (e *Egress) runJob(j *job) {
	if !j.claimed.CompareAndSwap(false, true) {
		return // Shutdown already finished it
	}
	defer e.finish(j)
	c := j.clip
	if c.Stream.isEvicted() {
		return // dropped or abandoned while queued; nothing was stored
	}

	// jctx ends on hard stop or when this clip's stream is evicted, so every
	// wait below is interruptible.
	jctx, cancel := context.WithCancel(e.hctx)
	defer cancel()
	go func() {
		select {
		case <-c.Stream.Evicted():
			e.noteStall(j)
			cancel()
		case <-jctx.Done():
		}
	}()

	var ow storage.ObjectWriter
	staged := false
	restarted := false
	for {
		err := e.upload(jctx, j, &ow, &staged)
		if err == nil {
			break
		}
		// The multipart upload vanished (NotFound). While the stream still
		// holds every byte, the clip is re-created once and replayed from
		// offset 0; nothing is lost, so this is no drop.
		if errors.Is(err, errUploadGone) && !restarted && c.Stream.releasedBytes() == 0 {
			restarted = true
			slog.Warn("egress: upload vanished; restarting the clip from the stream", "key", c.Keys.Video, "error", err)
			e.abortWriter(j, ow)
			ow = nil
			continue
		}
		e.abort(j, ow, staged, err)
		return
	}
	// complete runs on the hard-stop context, not on jctx: an eviction that
	// raced a Commit the server nevertheless completed must not cost the clip
	// its final sidecar (the video is committed and visible after recovery).
	// An evicted stream bounds it in real time, since nothing else ends it.
	cctx := e.hctx
	if c.Stream.isEvicted() {
		var ccancel context.CancelFunc
		cctx, ccancel = context.WithTimeout(e.hctx, abortTimeout)
		defer ccancel()
	}
	e.complete(cctx, j)
}

// upload runs the protocol up to and including the video commit.
func (e *Egress) upload(ctx context.Context, j *job, ow *storage.ObjectWriter, staged *bool) error {
	c := j.clip
	if err := e.wait(ctx); err != nil {
		return err
	}
	// Step 0: the staging sidecar precedes the first byte of the video.
	if !*staged {
		if err := e.persist(ctx, j, func(ctx context.Context) error {
			return e.cfg.Storage.PutSmall(ctx, c.Keys.StagingSidecar, c.Staging, "application/json")
		}); err != nil {
			return err
		}
		*staged = true
	}
	if err := e.persist(ctx, j, func(ctx context.Context) error {
		w, err := e.cfg.Storage.Create(ctx, c.Keys.Video)
		if err == nil {
			*ow = w
		}
		return err
	}); err != nil {
		return err
	}
	w := *ow

	buf := make([]byte, copyBufSize)
	var off int64
	var counted int64 // bytes already reported to UploadMetrics (the durable high-water mark)
	fails := 0        // consecutive failed Write or Resume calls
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := c.Stream.ReadAt(off, buf)
		if rerr != nil && rerr != io.EOF {
			return rerr // evicted
		}
		if n > 0 {
			before := w.DurableOffset()
			endOp := e.track(j)
			_, werr := w.Write(buf[:n])
			endOp()
			if werr != nil {
				fails++
				// A Write cancelled by eviction or shutdown says nothing
				// about storage unless it stalled; noteStall judged that.
				if ctx.Err() == nil {
					e.failure(werr)
				}
				// Wait for the breaker, back off, then let the driver
				// reposition itself and continue from the offset it reports.
				roff, err := e.resume(ctx, w, &fails)
				if err != nil {
					return err
				}
				off = roff
				counted = e.countDurable(counted, w.DurableOffset())
				c.Stream.Release(w.DurableOffset())
				continue
			}
			off += int64(n)
			// A Write that only buffered in the driver (the S3 driver copies
			// into its part buffer and touches the network once per part)
			// says nothing about storage. Only durable progress proves a
			// request reached it, so only that resets the breaker count and
			// the backoff; otherwise buffering Writes between failed part
			// uploads would mask an outage and spin on UploadPart.
			if durable := w.DurableOffset(); durable > before {
				fails = 0
				e.success()
				counted = e.countDurable(counted, durable)
				c.Stream.Release(durable)
			}
		}
		if rerr == io.EOF {
			break
		}
		if n == 0 {
			if err := c.Stream.Wait(ctx, off); err != nil {
				return err
			}
		}
	}

	if err := e.persist(ctx, j, w.Commit); err != nil {
		return err
	}
	e.countDurable(counted, off) // the tail that only Commit makes durable
	// The video is durable: mark it first so the accountant skips the clip,
	// then release its tail so it no longer counts as backlog. Neither the
	// accountant nor Shutdown evicts or reports the clip while the final
	// sidecar is still being stored. (An eviction that lands while Commit is
	// in flight is a legitimate drop at that moment; see runJob.)
	c.Committed.Store(true)
	j.committed.Store(true)
	c.Stream.Release(off)
	return nil
}

// resume repositions the driver after a failed Write and returns the offset to
// continue from. Resume is local in some drivers and cannot tell whether
// storage is back, so it never reports Success to the breaker: only a
// successful Write does. Every attempt first waits for the breaker and, from the
// second consecutive failure on, backs off, so an outage opens the
// breaker after FailureThreshold failures and the worker parks instead of
// spinning (design §6.3, relay-spec §4.8).
func (e *Egress) resume(ctx context.Context, w storage.ObjectWriter, fails *int) (int64, error) {
	for {
		if err := e.wait(ctx); err != nil {
			return 0, err
		}
		// The first failure resumes at once (a dropped connection is common);
		// each further consecutive failure backs off, 200 ms doubling to 2 s.
		if *fails > 1 {
			bound := min(retryBaseDelay<<min(*fails-2, 10), retryMaxDelay)
			if err := e.cfg.Clock.Sleep(ctx, fullJitter(bound)); err != nil {
				return 0, err
			}
		}
		off, err := w.Resume(ctx)
		if err == nil {
			if e.cfg.Metrics != nil {
				e.cfg.Metrics.UploadResume()
			}
			return off, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if e.cfg.Classify(err) == storage.NotFound {
			return 0, errors.Join(errUploadGone, err)
		}
		*fails++
		e.failure(err)
	}
}

// complete stores the final sidecar and removes the staging sidecar. The video
// is already committed, so failures here never drop the clip: a leftover
// staging sidecar is promoted by the recovery sweep (Contract §7.4).
func (e *Egress) complete(ctx context.Context, j *job) {
	c := j.clip
	data, err := c.Final()
	if err != nil {
		slog.Warn("egress: final sidecar not built; staging sidecar left for recovery", "key", c.Keys.Sidecar, "error", err)
		return
	}
	if err := e.persist(ctx, j, func(ctx context.Context) error {
		return e.cfg.Storage.PutSmall(ctx, c.Keys.Sidecar, data, "application/json")
	}); err != nil {
		slog.Warn("egress: final sidecar not stored; staging sidecar left for recovery", "key", c.Keys.Sidecar, "error", err)
		return
	}
	if e.cfg.OnCommitted != nil {
		e.cfg.OnCommitted(c.Camera, e.cfg.Clock.Now())
	}
	for range deleteStagingTries {
		err := e.do(ctx, func(ctx context.Context) error {
			return e.cfg.Storage.Delete(ctx, c.Keys.StagingSidecar)
		})
		if err == nil || e.cfg.Classify(err) == storage.NotFound {
			return
		}
		if ctx.Err() != nil {
			break
		}
	}
	slog.Warn("egress: staging sidecar not deleted; recovery will reap it", "key", c.Keys.StagingSidecar)
}

// abort ends a clip that will not be committed: the owner aborts the
// ObjectWriter and, when storage is reachable, removes the staging sidecar. The
// drop is recorded by whoever evicted the stream (backlog policy) or here at
// shutdown.
//
// The one other way a clip ends is a vanished upload (errUploadGone) that
// cannot be replayed because part of its bytes were already released.
// Relay-spec §4.8 says a clip is abandoned only by the backlog policy or at
// shutdown, and Contract §7.5.2 allows only stream_lost, backlog and shutdown
// as drop reasons, so no reason fits. The clip is logged at error level, its
// stream is evicted to free the backlog, and no OnDrop is emitted.
func (e *Egress) abort(j *job, ow storage.ObjectWriter, staged bool, err error) {
	switch {
	case e.hctx.Err() != nil:
		e.drop(j, DropShutdown)
	case !j.clip.Stream.isEvicted():
		slog.Error("egress: upload vanished and cannot be replayed; clip abandoned without a coverage reason",
			"key", j.clip.Keys.Video, "error", err)
		j.clip.Stream.Evict()
	}
	hard := e.hctx.Err() != nil

	// A fresh context: the clip's own is already cancelled. Real-time bound,
	// since storage is likely unreachable.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.hctx), abortTimeout)
	defer cancel()
	if ow != nil {
		if aerr := ow.Abort(ctx); aerr != nil {
			slog.Warn("egress: abort failed", "key", j.clip.Keys.Video, "error", aerr)
		}
	}
	// Best effort: skipped when storage is known to be down or the process is
	// stopping; the staging sidecar is invisible and recovery reaps it.
	if staged && !hard && e.breakerClosed() {
		if derr := e.cfg.Storage.Delete(ctx, j.clip.Keys.StagingSidecar); derr != nil && e.cfg.Classify(derr) != storage.NotFound {
			slog.Warn("egress: staging sidecar not deleted", "key", j.clip.Keys.StagingSidecar, "error", derr)
		}
	}
}

// abortWriter aborts ow (the owner's call) with a bounded fresh context.
func (e *Egress) abortWriter(j *job, ow storage.ObjectWriter) {
	if ow == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.hctx), abortTimeout)
	defer cancel()
	if aerr := ow.Abort(ctx); aerr != nil {
		slog.Warn("egress: abort failed", "key", j.clip.Keys.Video, "error", aerr)
	}
}

// wait parks until the circuit breaker is closed.
func (e *Egress) wait(ctx context.Context) error {
	if e.cfg.Breaker == nil {
		return ctx.Err()
	}
	return e.cfg.Breaker.Wait(ctx)
}

func (e *Egress) breakerClosed() bool {
	if e.cfg.Breaker == nil {
		return true
	}
	s, _ := e.cfg.Breaker.State()
	return s == Closed
}

func (e *Egress) success() {
	if e.cfg.Breaker != nil {
		e.cfg.Breaker.Success()
	}
}

// failure reports a failed streaming Write to the breaker. Write has no retry
// of its own; the resume that follows is the retry.
func (e *Egress) failure(err error) {
	e.uploadError(err)
	if e.cfg.Breaker != nil {
		e.cfg.Breaker.Failure(e.cfg.Classify(err) == storage.Permanent)
	}
}

// do runs op with the per-request retry and breaker accounting.
func (e *Egress) do(ctx context.Context, op func(ctx context.Context) error) error {
	err := Do(ctx, e.cfg.Clock, e.cfg.Classify, e.cfg.Breaker, op)
	if err != nil && ctx.Err() == nil {
		e.uploadError(err)
	}
	return err
}

// countDurable reports the bytes between the already reported high-water mark
// and durable, and returns the new mark.
func (e *Egress) countDurable(counted, durable int64) int64 {
	if durable <= counted {
		return counted
	}
	e.uploadBytes(durable - counted)
	return durable
}

// errUploadGone reports an operation that can never succeed (NotFound).
var errUploadGone = errors.New("egress: upload no longer exists")

// persist retries op until it succeeds or ctx ends: a clip is abandoned only
// by the backlog policy or at shutdown (relay-spec §4.8). The breaker paces
// the retries; without one a fixed delay does. A NotFound answer cannot be
// fixed by retrying and ends the clip.
func (e *Egress) persist(ctx context.Context, j *job, op func(ctx context.Context) error) error {
	for {
		err := e.do(ctx, func(ctx context.Context) error {
			defer e.track(j)()
			return op(ctx)
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.cfg.Classify(err) == storage.NotFound {
			return errors.Join(errUploadGone, err)
		}
		if e.cfg.Breaker == nil {
			if serr := e.cfg.Clock.Sleep(ctx, retryMaxDelay); serr != nil {
				return serr
			}
		}
	}
}

// track marks a storage request of j as pending and returns the function that
// clears the mark.
func (e *Egress) track(j *job) (end func()) {
	j.pendingSince.Store(e.cfg.Clock.Now().UnixNano())
	return func() { j.pendingSince.Store(0) }
}

// noteStall is called when the backlog policy is about to cancel j's clip. A
// request still pending after stallReport is a failure signal for the breaker:
// under sustained overload the oldest finalized clip, the one a worker is
// draining, is evicted every few seconds, so during an outage its requests
// are cancelled before the transport times out and would never be reported.
// Without this the breaker never opens and the workers keep hammering a dead
// endpoint. A quick cancellation proves nothing and is not reported.
func (e *Egress) noteStall(j *job) {
	since := j.pendingSince.Load()
	if since == 0 || e.cfg.Breaker == nil {
		return
	}
	if e.cfg.Clock.Now().Sub(time.Unix(0, since)) >= stallReport {
		e.cfg.Breaker.Failure(false)
	}
}

// stallReport is how long a storage request may be pending when the backlog
// policy cancels it for the cancellation to count as a breaker failure.
const stallReport = 5 * time.Second
