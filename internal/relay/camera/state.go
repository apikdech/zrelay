package camera

import (
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

// State is a camera's lifecycle state (relay-spec §4.1, design §4.2):
//
//	connecting -> priming -> ready -> degraded -> connecting
type State string

// Camera states.
const (
	// StateConnecting: the RTSP session is being set up.
	StateConnecting State = "connecting"
	// StatePriming: the RTSP session is up but has not delivered an IDR.
	StatePriming State = "priming"
	// StateReady: RTSP has delivered at least one IDR since connecting and
	// the event subscription is active.
	StateReady State = "ready"
	// StateDegraded: RTSP or events are down. Video keeps flowing to the
	// Hub whenever RTSP is up.
	StateDegraded State = "degraded"
)

// Link health values of Status.RTSP and Status.Events.
const (
	linkUp       = "up"
	linkDown     = "down"
	linkDegraded = "degraded"
)

// Status is a camera's state and the health of its two links.
type Status struct {
	State State
	// RTSP is "up" once an IDR has arrived on the current session,
	// "degraded" while the session is up without one, else "down".
	RTSP string
	// Events is "up" while the event subscription is active, else "down".
	Events string
	// Since is when State last changed.
	Since time.Time
}

// Camera is the handle to a supervised camera. Hub carries its video and
// audio, Tracker holds the trigger state its events produce.
type Camera struct {
	ID      string
	Hub     *Hub
	Tracker *events.Tracker
	// Status returns the current status. It is safe for concurrent use.
	Status func() Status
}
