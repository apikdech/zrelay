package status

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/egress"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// DefaultInterval is the status heartbeat period (Contract §7.5).
const DefaultInterval = 60 * time.Second

// Config configures a Publisher.
type Config struct {
	Storage storage.Writer
	// Breaker gates all writes: while it is not Closed the publisher skips
	// them, coverage keeps accumulating in memory, and the next tick after it
	// closes writes the current state (design §6.6). Nil means always closed
	// and omits the storage field from status/relay.json.
	Breaker *egress.Breaker
	RelayID string
	Version string
	KeyID   [8]byte
	// Cameras returns the current per-camera status, one entry per configured
	// camera.
	Cameras func() []sidecar.CameraStatus
	// Coverage maps a camera id to its tracker.
	Coverage map[string]*CoverageTracker
	// Backlog returns the bytes currently held in the egress backlog.
	Backlog func() int64
	Clock   clock.Clock
	// Interval is the heartbeat period; zero means DefaultInterval.
	Interval time.Duration
	// Logger receives write failures; nil means slog.Default().
	Logger *slog.Logger
}

// object is a serialised status object waiting to be written.
type object struct {
	key  string
	data []byte
}

// Publisher writes status/relay.json every Interval and one coverage object
// per camera for each completed UTC hour, all via PutSmall (relay-spec §4.10).
// A failed write is logged and retried at the next tick.
type Publisher struct {
	cfg Config
	clk clock.Clock
	log *slog.Logger
	ids []string // camera ids with a tracker, sorted

	mu       sync.Mutex // serialises tick and Flush
	nextHour time.Time  // first hour whose coverage has not been collected
	pending  []object   // coverage taken from the trackers, not yet written
}

// New returns a Publisher. Coverage is published for hours that end after
// New is called.
func New(cfg Config) *Publisher {
	p := &Publisher{cfg: cfg, clk: cfg.Clock, log: cfg.Logger}
	if p.clk == nil {
		p.clk = clock.Real
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.cfg.Interval <= 0 {
		p.cfg.Interval = DefaultInterval
	}
	for id := range cfg.Coverage {
		p.ids = append(p.ids, id)
	}
	slices.Sort(p.ids)
	p.nextHour = p.clk.Now().UTC().Truncate(time.Hour)
	return p
}

// Run publishes immediately, then every Interval, until ctx is done.
func (p *Publisher) Run(ctx context.Context) {
	p.tick(ctx)
	t := p.clk.NewTimer(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			p.tick(ctx)
			t.Reset(p.cfg.Interval)
		}
	}
}

// Flush writes the coverage of every completed hour and of the running hour
// up to now. It is called once on shutdown. It does nothing while the breaker
// is not Closed: coverage cannot be published while storage is down.
func (p *Publisher) Flush(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.storageClosed() {
		p.log.Warn("status: storage breaker not closed, partial coverage not written")
		return
	}
	now := p.clk.Now().UTC()
	p.collectCompleted(now)
	p.enqueue(now.Truncate(time.Hour))
	p.writePending(ctx)
}

func (p *Publisher) tick(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.storageClosed() {
		p.log.Debug("status: storage breaker not closed, tick skipped")
		return
	}
	p.writeStatus(ctx)
	p.collectCompleted(p.clk.Now().UTC())
	p.writePending(ctx)
}

func (p *Publisher) storageClosed() bool {
	if p.cfg.Breaker == nil {
		return true
	}
	s, _ := p.cfg.Breaker.State()
	return s == egress.Closed
}

func (p *Publisher) writeStatus(ctx context.Context) {
	s := sidecar.Status{
		Schema:    sidecar.StatusSchema,
		RelayID:   p.cfg.RelayID,
		Version:   p.cfg.Version,
		UpdatedAt: sidecar.Time{Time: p.clk.Now().UTC()},
		KeyID:     hex.EncodeToString(p.cfg.KeyID[:]),
		Cameras:   []sidecar.CameraStatus{},
	}
	if p.cfg.Cameras != nil {
		s.Cameras = append(s.Cameras, p.cfg.Cameras()...)
	}
	if b := p.cfg.Breaker; b != nil {
		state, since := b.State()
		st := &sidecar.StorageStatus{State: stateName(state), Since: sidecar.Time{Time: since.UTC()}}
		if p.cfg.Backlog != nil {
			st.BacklogBytes = p.cfg.Backlog()
		}
		s.Storage = st
	}
	data, err := json.Marshal(s)
	if err != nil {
		p.log.Error("status: marshal relay status", "err", err)
		return
	}
	p.put(ctx, object{sidecar.StatusKey, data})
}

func stateName(s egress.BreakerState) string {
	switch s {
	case egress.Closed:
		return "closed"
	case egress.HalfOpen:
		return "half_open"
	default:
		return "open"
	}
}

// collectCompleted takes the coverage of every hour that ended at or before
// now and has not been collected yet.
func (p *Publisher) collectCompleted(now time.Time) {
	for !p.nextHour.Add(time.Hour).After(now) {
		p.enqueue(p.nextHour)
		p.nextHour = p.nextHour.Add(time.Hour)
	}
}

// enqueue takes the given hour from every tracker and queues the non-empty
// results. An hour with no object counts as not ready (Contract §7.5.2), so
// an hour with neither ready intervals nor drops is not written.
func (p *Publisher) enqueue(hour time.Time) {
	for _, id := range p.ids {
		c := p.cfg.Coverage[id].TakeHour(hour)
		if len(c.Ready) == 0 && len(c.Dropped) == 0 {
			continue
		}
		c.CameraID = id
		if err := c.Validate(); err != nil {
			p.log.Error("status: coverage invalid, discarded", "camera", id, "hour", hour, "err", err)
			continue
		}
		data, err := json.Marshal(c)
		if err != nil {
			p.log.Error("status: marshal coverage", "camera", id, "hour", hour, "err", err)
			continue
		}
		p.pending = append(p.pending, object{sidecar.CoverageKey(id, hour), data})
	}
}

// writePending writes queued coverage in order and stops at the first
// failure, keeping the rest for the next tick.
func (p *Publisher) writePending(ctx context.Context) {
	for len(p.pending) > 0 {
		if !p.put(ctx, p.pending[0]) {
			return
		}
		p.pending = p.pending[1:]
	}
}

func (p *Publisher) put(ctx context.Context, o object) bool {
	if err := p.cfg.Storage.PutSmall(ctx, o.key, o.data, "application/json"); err != nil {
		p.log.Warn("status: write failed, retrying at the next tick", "key", o.key, "err", err)
		return false
	}
	return true
}
