package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"

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
			cancel()
		case <-jctx.Done():
		}
	}()

	var ow storage.ObjectWriter
	staged := false
	if err := e.upload(jctx, j, &ow, &staged); err != nil {
		e.abort(j, ow, staged, err)
		return
	}
	e.complete(jctx, j)
}

// upload runs the protocol up to and including the video commit.
func (e *Egress) upload(ctx context.Context, j *job, ow *storage.ObjectWriter, staged *bool) error {
	c := j.clip
	if err := e.wait(ctx); err != nil {
		return err
	}
	// Step 0: the staging sidecar precedes the first byte of the video.
	if err := e.persist(ctx, func(ctx context.Context) error {
		return e.cfg.Storage.PutSmall(ctx, c.Keys.StagingSidecar, c.Staging, "application/json")
	}); err != nil {
		return err
	}
	*staged = true
	if err := e.persist(ctx, func(ctx context.Context) error {
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
	for {
		n, rerr := c.Stream.ReadAt(off, buf)
		if rerr != nil && rerr != io.EOF {
			return rerr // evicted
		}
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				e.failure(werr)
				// Wait for the breaker, then let the driver reposition itself
				// and continue from the offset it reports.
				var roff int64
				if err := e.persist(ctx, func(ctx context.Context) error {
					var err error
					roff, err = w.Resume(ctx)
					return err
				}); err != nil {
					return err
				}
				off = roff
				c.Stream.Release(w.DurableOffset())
				continue
			}
			e.success()
			off += int64(n)
			c.Stream.Release(w.DurableOffset())
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

	if err := e.persist(ctx, w.Commit); err != nil {
		return err
	}
	j.committed.Store(true)
	return nil
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
	if err := e.persist(ctx, func(ctx context.Context) error {
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
// drop is recorded by whoever evicted the stream; a failure the retries cannot
// fix (err not from cancellation) is recorded here.
func (e *Egress) abort(j *job, ow storage.ObjectWriter, staged bool, err error) {
	switch {
	case e.hctx.Err() != nil:
		e.drop(j, DropShutdown)
	case !j.clip.Stream.isEvicted():
		slog.Error("egress: upload failed permanently", "key", j.clip.Keys.Video, "error", err)
		e.drop(j, DropError)
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
	if e.cfg.Breaker != nil {
		e.cfg.Breaker.Failure(e.cfg.Classify(err) == storage.Permanent)
	}
}

// do runs op with the per-request retry and breaker accounting.
func (e *Egress) do(ctx context.Context, op func(ctx context.Context) error) error {
	return Do(ctx, e.cfg.Clock, e.cfg.Classify, e.cfg.Breaker, op)
}

// errUploadGone reports an operation that can never succeed (NotFound).
var errUploadGone = errors.New("egress: upload no longer exists")

// persist retries op until it succeeds or ctx ends: a clip is abandoned only
// by the backlog policy or at shutdown (relay-spec §4.8). The breaker paces
// the retries; without one a fixed delay does. A NotFound answer cannot be
// fixed by retrying and ends the clip.
func (e *Egress) persist(ctx context.Context, op func(ctx context.Context) error) error {
	for {
		err := e.do(ctx, op)
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
