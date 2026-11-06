package egress

import (
	"context"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

const (
	retryAttempts  = 3
	retryBaseDelay = 200 * time.Millisecond
	retryMaxDelay  = 2 * time.Second
)

// Do runs op with up to 3 attempts and full-jitter backoff (200 ms base, 2 s
// cap), using classify to decide whether an error is worth retrying
// (design §6.3). Transient errors are retried; Permanent and NotFound are
// returned at once.
//
// Before every attempt Do waits for b to be Closed, so parked workers do not
// send requests while the breaker is open. The final outcome is reported to b
// exactly once: Success on success or NotFound (storage answered), Failure on
// exhausted transient retries, Failure(permanent) on a permanent error. If ctx
// ends, nothing is reported, since shutdown is not a storage failure. b may be
// nil.
func Do(ctx context.Context, clk clock.Clock, classify func(error) storage.ErrorClass, b *Breaker, op func(ctx context.Context) error) error {
	var err error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			bound := min(retryBaseDelay<<(attempt-1), retryMaxDelay)
			if serr := clk.Sleep(ctx, fullJitter(bound)); serr != nil {
				return serr
			}
		}
		if b != nil {
			if werr := b.Wait(ctx); werr != nil {
				return werr
			}
		}
		if err = op(ctx); err == nil {
			if b != nil {
				b.Success()
			}
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		switch classify(err) {
		case storage.Permanent:
			if b != nil {
				b.Failure(true)
			}
			return err
		case storage.NotFound:
			if b != nil {
				b.Success()
			}
			return err
		}
	}
	if b != nil {
		b.Failure(false)
	}
	return err
}
