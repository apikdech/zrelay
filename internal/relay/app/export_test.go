package app

import (
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

// SetEventSource replaces how Run builds a camera's event source and returns
// the function that restores it.
func SetEventSource(f func(config.Camera, []events.Rule, clock.Clock) events.Source) (restore func()) {
	old := newEventSource
	newEventSource = f
	return func() { newEventSource = old }
}

// SetRecoveryRetryEvery shortens the interval between startup recovery
// retries for a test and returns the function that restores it.
func SetRecoveryRetryEvery(d time.Duration) (restore func()) {
	old := recoveryRetryEvery
	recoveryRetryEvery = d
	return func() { recoveryRetryEvery = old }
}

// GCPercent is the garbage collector target Run sets.
const GCPercent = gcPercent

// ViewerQueues is the per-camera viewer queue map Run hands to live view and
// restream.
var ViewerQueues = viewerQueues
