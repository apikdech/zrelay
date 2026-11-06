// Package events turns camera event notifications into trigger signals: the
// ONVIF PullPoint client, the rules that classify notifications, and the
// per-camera trigger tracker (relay-spec §4.4, design §4.3).
package events

import (
	"context"
	"time"
)

// Kind classifies a trigger.
type Kind string

// Trigger kinds.
const (
	KindMotion  Kind = "motion"
	KindPerson  Kind = "person"
	KindVehicle Kind = "vehicle"
	KindPet     Kind = "pet"
	KindOther   Kind = "other"
)

// allKinds lists every Kind.
var allKinds = []Kind{KindMotion, KindPerson, KindVehicle, KindPet, KindOther}

// Signal is one change of a trigger kind reported by a camera.
type Signal struct {
	Camera string
	Kind   Kind
	Active bool
	// At is the host time of receipt, or the message UtcTime moved to host
	// time by the camera clock offset when the message has one.
	At       time.Time
	RawTopic string
	// Initialized marks a PropertyOperation="Initialized" message: the
	// current state after (re)subscribe, not a new change.
	Initialized bool
}

// Source produces Signals for one camera.
type Source interface {
	// Run blocks until ctx is done, emitting signals. It MUST reconnect
	// internally and report readiness changes via the ready callback.
	Run(ctx context.Context, emit func(Signal), ready func(bool)) error
}
