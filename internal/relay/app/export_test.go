package app

import "time"

// SetRecoveryRetryEvery shortens the interval between startup recovery
// retries for a test and returns the function that restores it.
func SetRecoveryRetryEvery(d time.Duration) (restore func()) {
	old := recoveryRetryEvery
	recoveryRetryEvery = d
	return func() { recoveryRetryEvery = old }
}

// GCPercent is the garbage collector target Run sets.
const GCPercent = gcPercent
