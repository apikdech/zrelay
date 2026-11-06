package camera

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

// expireEvery is how often the supervisor lets the tracker force quiet
// kinds inactive.
const expireEvery = time.Second

// defaultTriggerTimeout applies when SuperviseConfig.TriggerTimeout is not
// set (config default trigger_timeout_seconds).
const defaultTriggerTimeout = 60 * time.Second

// SuperviseConfig configures Supervise.
type SuperviseConfig struct {
	ID      string
	RTSPURL string
	// Events is the camera's event source. Without one the camera never
	// becomes ready: ready needs an active event subscription.
	Events events.Source
	// TriggerTimeout is the tracker's trigger_timeout_seconds; it defaults
	// to 60 s.
	TriggerTimeout time.Duration
	// Clock defaults to clock.Real. It times reconnect backoff and tracker
	// expiry.
	Clock clock.Clock
	// OnState, if set, is called with the initial status and after every
	// change of State, RTSP or Events, in order. It must not block.
	OnState func(Status)
	Backoff BackoffConfig
}

// rtspPhase is where the current RTSP attempt stands.
type rtspPhase int

const (
	phaseConnecting rtspPhase = iota // dialling or negotiating
	phaseSession                     // codec received, no IDR yet
	phaseIDR                         // at least one IDR delivered
	phaseLost                        // the session ended; waiting to retry
)

// supervisor owns the RTSP loop, the event loop and the status of one camera.
type supervisor struct {
	cfg     SuperviseConfig
	clk     clock.Clock
	hub     *Hub
	tracker *events.Tracker

	notify sync.Mutex // serializes OnState calls; taken before mu
	mu     sync.Mutex
	phase  rtspPhase
	evUp   bool
	status Status

	idr atomic.Bool // an IDR was delivered on the current session
}

// Supervise starts the RTSP loop and the event loop of one camera as
// goroutines and returns at once. Both reconnect with full-jitter
// exponential backoff until ctx is done. Nothing is shared between
// Supervise calls, so one camera's failure never affects another.
//
// The camera is ready when RTSP has delivered at least one IDR since it
// connected and the event subscription is active. If RTSP is up and events
// are down it is degraded and the Hub keeps carrying video.
func Supervise(ctx context.Context, cfg SuperviseConfig) *Camera {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real
	}
	if cfg.TriggerTimeout <= 0 {
		cfg.TriggerTimeout = defaultTriggerTimeout
	}
	s := &supervisor{
		cfg:     cfg,
		clk:     cfg.Clock,
		hub:     NewHub(cfg.ID),
		tracker: events.NewTracker(cfg.TriggerTimeout, cfg.Clock),
	}
	s.status = Status{State: StateConnecting, RTSP: linkDown, Events: linkDown, Since: s.clk.Now()}
	if cfg.OnState != nil {
		cfg.OnState(s.status)
	}
	go s.rtspLoop(ctx)
	if cfg.Events != nil {
		go s.eventsLoop(ctx)
	}
	go s.expireLoop(ctx)
	return &Camera{ID: cfg.ID, Hub: s.hub, Tracker: s.tracker, Status: s.Status}
}

// Status returns the current status.
func (s *supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// update applies mutate to the link state and, when the status changed,
// stores and reports it.
func (s *supervisor) update(mutate func()) {
	s.notify.Lock()
	defer s.notify.Unlock()
	s.mu.Lock()
	mutate()
	next := s.compute()
	if next == s.status {
		s.mu.Unlock()
		return
	}
	if next.State != s.status.State {
		next.Since = s.clk.Now()
	}
	s.status = next
	s.mu.Unlock()
	if s.cfg.OnState != nil {
		s.cfg.OnState(next)
	}
}

// compute derives the status from the link state. s.mu must be held.
func (s *supervisor) compute() Status {
	st := Status{Events: linkDown, Since: s.status.Since}
	if s.evUp {
		st.Events = linkUp
	}
	switch s.phase {
	case phaseConnecting:
		st.State, st.RTSP = StateConnecting, linkDown
	case phaseSession:
		st.State, st.RTSP = StatePriming, linkDegraded
	case phaseIDR:
		st.RTSP = linkUp
		st.State = StateDegraded
		if s.evUp {
			st.State = StateReady
		}
	case phaseLost:
		st.State, st.RTSP = StateDegraded, linkDown
	}
	return st
}

func (s *supervisor) setPhase(p rtspPhase) { s.update(func() { s.phase = p }) }

// rtspLoop runs RTSP sessions until ctx is done.
func (s *supervisor) rtspLoop(ctx context.Context) {
	b := newBackoff(s.cfg.Backoff, nil)
	for {
		s.idr.Store(false)
		s.setPhase(phaseConnecting)
		start := s.clk.Now()
		err := RunRTSP(ctx, s.cfg.RTSPURL, &statusSink{s: s})
		s.hub.Lost()
		if ctx.Err() != nil {
			return
		}
		s.setPhase(phaseLost)
		delay := b.next(s.clk.Now().Sub(start))
		// err never holds credentials: RunRTSP redacts them.
		slog.Warn("rtsp session lost", "camera", s.cfg.ID, "err", err, "retry_in", delay)
		if s.clk.Sleep(ctx, delay) != nil {
			return
		}
	}
}

// statusSink feeds the Hub and reports session progress to the supervisor.
type statusSink struct{ s *supervisor }

func (k *statusSink) Codec(c Codec, ts TimeSource) {
	k.s.hub.Codec(c, ts)
	k.s.update(func() {
		if k.s.phase == phaseConnecting {
			k.s.phase = phaseSession
		}
	})
}

func (k *statusSink) Video(au *AccessUnit) {
	k.s.hub.Video(au)
	if au.IDR && !k.s.idr.Swap(true) {
		k.s.setPhase(phaseIDR)
	}
}

func (k *statusSink) Audio(p *AudioPacket) { k.s.hub.Audio(p) }

// eventsLoop runs the event source until ctx is done. Sources reconnect
// internally; this loop only guards against one that returns early.
func (s *supervisor) eventsLoop(ctx context.Context) {
	b := newBackoff(s.cfg.Backoff, nil)
	emit := func(sig events.Signal) { s.tracker.Apply(sig) }
	ready := func(up bool) {
		if !up {
			s.tracker.ResetAll()
		}
		s.update(func() { s.evUp = up })
	}
	for {
		start := s.clk.Now()
		err := s.cfg.Events.Run(ctx, emit, ready)
		ready(false)
		if ctx.Err() != nil {
			return
		}
		delay := b.next(s.clk.Now().Sub(start))
		slog.Warn("event source stopped", "camera", s.cfg.ID, "err", err, "retry_in", delay)
		if s.clk.Sleep(ctx, delay) != nil {
			return
		}
	}
}

// expireLoop forces kinds inactive whose inactive message never came.
func (s *supervisor) expireLoop(ctx context.Context) {
	t := s.clk.NewTimer(expireEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			s.tracker.Expire()
			t.Reset(expireEvery)
		}
	}
}
