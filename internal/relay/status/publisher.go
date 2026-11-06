package status

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	// Reader, when set, lets the publisher merge an hour's coverage with the
	// object already stored for it (a partial hour written by an earlier
	// process or by Flush) instead of replacing it. Nil means the object is
	// replaced.
	Reader ObjectReader
	// Classify maps a storage error to its class so write outcomes can be
	// reported to Breaker. Nil treats every error as transient.
	Classify func(error) storage.ErrorClass
	RelayID  string
	Version  string
	KeyID    [8]byte
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

// ObjectReader is the read side the publisher needs; storage.Reader
// satisfies it. A missing object must be reported as storage.ErrNotFound.
type ObjectReader interface {
	OpenRange(ctx context.Context, key string, off, n int64) (*storage.ObjectRange, error)
}

// object is a serialised status object waiting to be written. cov is set for
// coverage: at write time it is merged with the object already stored.
type object struct {
	key  string
	data []byte
	cov  *sidecar.Coverage
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
	p.put(ctx, sidecar.StatusKey, data)
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
		p.pending = append(p.pending, object{key: sidecar.CoverageKey(id, hour), data: data, cov: &c})
	}
}

// writePending writes queued coverage in order and stops at the first
// failure, keeping the rest for the next tick.
func (p *Publisher) writePending(ctx context.Context) {
	for len(p.pending) > 0 {
		if !p.putCoverage(ctx, p.pending[0]) {
			return
		}
		p.pending = p.pending[1:]
	}
}

// putCoverage writes one coverage object. When a Reader is configured it
// first reads the object already stored for that camera and hour and writes
// the union of the two, so a partial hour from an earlier process or from
// Flush is extended rather than replaced. If the stored object cannot be read
// nothing is written: the write is retried at the next tick and the stored
// object survives. A stored object that is not a valid coverage object for
// the same camera and hour is replaced.
func (p *Publisher) putCoverage(ctx context.Context, o object) bool {
	data := o.data
	if p.cfg.Reader != nil && o.cov != nil {
		if !p.storageClosed() {
			return false
		}
		stored, err := p.readCoverage(ctx, o.key, o.cov)
		if err != nil {
			p.report(ctx, err)
			p.log.Warn("status: reading stored coverage failed, retrying at the next tick", "key", o.key, "err", err)
			return false
		}
		p.report(ctx, nil)
		if stored != nil {
			merged := mergeCoverage(*stored, *o.cov)
			if err := merged.Validate(); err != nil {
				p.log.Error("status: merged coverage invalid, stored object replaced", "key", o.key, "err", err)
			} else if b, err := json.Marshal(merged); err != nil {
				p.log.Error("status: marshal merged coverage", "key", o.key, "err", err)
			} else {
				data = b
			}
		}
	}
	return p.put(ctx, o.key, data)
}

// readCoverage returns the valid coverage object stored at key for the same
// camera and hour as want, or nil when there is none, it is unreadable as
// JSON, or it belongs to another camera or hour. A non-nil error means the
// storage request itself failed.
func (p *Publisher) readCoverage(ctx context.Context, key string, want *sidecar.Coverage) (*sidecar.Coverage, error) {
	rc, err := p.cfg.Reader.OpenRange(ctx, key, 0, -1)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, storage.MaxSmallSize+1))
	if err != nil {
		return nil, err
	}
	var c sidecar.Coverage
	if len(b) > storage.MaxSmallSize || json.Unmarshal(b, &c) != nil || c.Validate() != nil ||
		c.CameraID != want.CameraID || !c.HourStart.Equal(want.HourStart.Time) {
		p.log.Warn("status: stored coverage is not a valid object for this camera and hour, replacing it", "key", key)
		return nil, nil
	}
	return &c, nil
}

// put writes one small object and reports the outcome to the breaker, so an
// idle relay (no drain worker making requests) still opens it, and with it
// /healthz and the breaker metric, when storage is down.
func (p *Publisher) put(ctx context.Context, key string, data []byte) bool {
	if !p.storageClosed() {
		return false
	}
	err := p.cfg.Storage.PutSmall(ctx, key, data, "application/json")
	p.report(ctx, err)
	if err != nil {
		p.log.Warn("status: write failed, retrying at the next tick", "key", key, "err", err)
		return false
	}
	return true
}

// report tells the breaker how a storage request ended, as egress.Do does:
// success and "not found" mean storage answered, a permanent error opens the
// breaker at once, any other error counts as a transient failure. Nothing is
// reported once ctx is done, since shutdown is not a storage failure.
func (p *Publisher) report(ctx context.Context, err error) {
	b := p.cfg.Breaker
	if b == nil || ctx.Err() != nil {
		return
	}
	if err == nil {
		b.Success()
		return
	}
	class := storage.Transient
	if p.cfg.Classify != nil {
		class = p.cfg.Classify(err)
	}
	switch class {
	case storage.NotFound:
		b.Success()
	case storage.Permanent:
		b.Failure(true)
	default:
		b.Failure(false)
	}
}
