package status_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/egress"
	"github.com/apikdech/zrelay-v2/internal/relay/status"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// memStore is an in-memory storage.Writer that records PutSmall calls.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    chan string // one entry per successful PutSmall
	fail    int         // the next fail PutSmall calls return an error
	readErr error       // error returned by OpenRange when set
}

func newMemStore() *memStore {
	return &memStore{objects: map[string][]byte{}, puts: make(chan string, 1024)}
}

func (m *memStore) PutSmall(_ context.Context, key string, data []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail > 0 {
		m.fail--
		return errors.New("boom")
	}
	m.objects[key] = append([]byte(nil), data...)
	m.puts <- key
	return nil
}

// OpenRange makes memStore a status.ObjectReader.
func (m *memStore) OpenRange(_ context.Context, key string, off, n int64) (*storage.ObjectRange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, m.readErr
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	b = b[off:]
	return &storage.ObjectRange{ReadCloser: io.NopCloser(bytes.NewReader(b)), Total: int64(len(b))}, nil
}

func (m *memStore) Create(context.Context, string) (storage.ObjectWriter, error) {
	return nil, errors.New("not used")
}
func (m *memStore) Delete(context.Context, string) error { return errors.New("not used") }

func (m *memStore) setFail(n int) {
	m.mu.Lock()
	m.fail = n
	m.mu.Unlock()
}

func (m *memStore) get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	return b, ok
}

func (m *memStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *memStore) waitPut(t *testing.T, key string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case k := <-m.puts:
			if k == key {
				return
			}
		case <-timeout:
			t.Fatalf("no PutSmall of %s", key)
		}
	}
}

type rig struct {
	t     *testing.T
	clk   *clock.Fake
	store *memStore
	cov   *status.CoverageTracker
	pub   *status.Publisher
	brk   *egress.Breaker
	bclk  *clock.Fake
}

var keyID = [8]byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07, 0x18}

// newRig builds a rig whose breaker tolerates many consecutive write failures,
// so a failed write is simply retried at the next tick.
func newRig(t *testing.T, start time.Time) *rig {
	t.Helper()
	return newRigThreshold(t, start, 100)
}

func newRigThreshold(t *testing.T, start time.Time, threshold int) *rig {
	t.Helper()
	r := &rig{t: t, clk: clock.NewFake(start), store: newMemStore(), bclk: clock.NewFake(start)}
	r.cov = status.NewCoverageTracker(r.clk)
	// The breaker has its own fake clock so its recovery timers never
	// interfere with the publisher's tick timer.
	r.brk = egress.NewBreaker(egress.BreakerConfig{FailureThreshold: threshold, BaseDelay: time.Second, MaxDelay: time.Second},
		r.bclk, func(context.Context) error { return nil }, nil)
	r.pub = status.New(status.Config{
		Storage: r.store,
		Reader:  r.store,
		Breaker: r.brk,
		RelayID: "home-mini",
		Version: "2.0.0",
		KeyID:   keyID,
		Cameras: func() []sidecar.CameraStatus {
			return []sidecar.CameraStatus{{ID: "front-door", Name: "Front Door", RTSP: "up", Events: "up", Since: sidecar.Time{Time: start}}}
		},
		Coverage: map[string]*status.CoverageTracker{"front-door": r.cov},
		Backlog:  func() int64 { return 1234 },
		Clock:    r.clk,
		Interval: 60 * time.Second,
	})
	return r
}

// run starts the publisher and returns a stop function.
func (r *rig) run() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.pub.Run(ctx) }()
	return func() { cancel(); <-done }
}

// step advances the publisher clock by d and waits until the tick it caused
// has finished (the tick timer is armed again).
func (r *rig) step(d time.Duration) {
	r.clk.Advance(d)
	r.clk.BlockUntil(1)
}

func (r *rig) openBreaker() {
	r.brk.Failure(true)
	if s, _ := r.brk.State(); s != egress.Open {
		r.t.Fatalf("breaker state = %v, want Open", s)
	}
}

func (r *rig) closeBreaker() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.brk.Run(ctx)
	r.bclk.BlockUntil(1)
	r.bclk.Advance(time.Second)
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	if err := r.brk.Wait(wctx); err != nil {
		r.t.Fatalf("breaker did not close: %v", err)
	}
}

func decodeStatus(t *testing.T, b []byte) sidecar.Status {
	t.Helper()
	var s sidecar.Status
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, b)
	}
	return s
}

func TestRelayJSONEvery60s(t *testing.T) {
	r := newRig(t, utc(14, 59, 0))
	stop := r.run()
	defer stop()

	r.store.waitPut(t, sidecar.StatusKey)
	r.clk.BlockUntil(1)
	for i := 1; i <= 3; i++ {
		r.step(60 * time.Second)
		r.store.waitPut(t, sidecar.StatusKey)
		raw, ok := r.store.get(sidecar.StatusKey)
		if !ok {
			t.Fatal("status/relay.json missing")
		}
		s := decodeStatus(t, raw)
		if s.Schema != sidecar.StatusSchema {
			t.Fatalf("schema = %q", s.Schema)
		}
		if want := utc(14, 59, 0).Add(time.Duration(i) * 60 * time.Second); !s.UpdatedAt.Equal(want) {
			t.Fatalf("tick %d updated_at = %v, want %v", i, s.UpdatedAt, want)
		}
		if s.RelayID != "home-mini" || s.Version != "2.0.0" || s.KeyID != "a1b2c3d4e5f60718" {
			t.Fatalf("identity fields wrong: %+v", s)
		}
		if len(s.Cameras) != 1 || s.Cameras[0].ID != "front-door" || s.Cameras[0].RTSP != "up" {
			t.Fatalf("cameras = %+v", s.Cameras)
		}
		if s.Storage == nil || s.Storage.State != "closed" || s.Storage.BacklogBytes != 1234 {
			t.Fatalf("storage = %+v, want state closed backlog 1234", s.Storage)
		}
		if _, since := r.brk.State(); !s.Storage.Since.Equal(since) {
			t.Fatalf("storage.since = %v, want %v", s.Storage.Since, since)
		}
	}
}

func TestCoverageWrittenAtHourEnd(t *testing.T) {
	r := newRig(t, utc(14, 0, 0))
	stop := r.run()
	defer stop()
	r.store.waitPut(t, sidecar.StatusKey)
	r.clk.BlockUntil(1)

	r.cov.SetReady(true, utc(14, 0, 0))
	r.cov.Drop(utc(14, 37, 10), "stream_lost", 52000)
	r.cov.SetReady(false, utc(14, 37, 10))
	r.cov.SetReady(true, utc(14, 38, 2)) // still ready when the hour ends

	r.step(time.Hour) // 15:00:00, one tick
	key := sidecar.CoverageKey("front-door", utc(14, 0, 0))
	r.store.waitPut(t, key)
	raw, _ := r.store.get(key)
	var c sidecar.Coverage
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("coverage JSON: %v\n%s", err, raw)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("coverage invalid: %v\n%s", err, raw)
	}
	if c.CameraID != "front-door" || !c.HourStart.Equal(utc(14, 0, 0)) {
		t.Fatalf("coverage identity: %+v", c)
	}
	wantIvs(t, c,
		[2]time.Time{utc(14, 0, 0), utc(14, 37, 10)},
		[2]time.Time{utc(14, 38, 2), utc(15, 0, 0)}) // clamped to the hour end
	if len(c.Dropped) != 1 || c.Dropped[0].Reason != "stream_lost" || c.Dropped[0].ApproxMs != 52000 {
		t.Fatalf("dropped = %+v", c.Dropped)
	}
	if _, ok := r.store.get(sidecar.CoverageKey("front-door", utc(15, 0, 0))); ok {
		t.Fatal("coverage for the running hour was written before the hour ended")
	}
}

func TestSkipsWhileBreakerOpenAndCatchesUp(t *testing.T) {
	r := newRig(t, utc(14, 30, 0))
	r.cov.SetReady(true, utc(14, 30, 0))
	r.openBreaker()
	stop := r.run()
	defer stop()
	r.clk.BlockUntil(1) // first tick done (skipped)

	r.step(35 * time.Minute) // 15:05, the 14:00 hour has ended while open
	if keys := r.store.keys(); len(keys) != 0 {
		t.Fatalf("wrote %v while the breaker was open", keys)
	}

	r.closeBreaker()
	r.step(60 * time.Second) // 15:06
	key := sidecar.CoverageKey("front-door", utc(14, 0, 0))
	raw, ok := r.store.get(key)
	if !ok {
		t.Fatalf("hour 14 coverage not written after the breaker closed; have %v", r.store.keys())
	}
	var c sidecar.Coverage
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	wantIvs(t, c, [2]time.Time{utc(14, 30, 0), utc(15, 0, 0)})
	raw, ok = r.store.get(sidecar.StatusKey)
	if !ok {
		t.Fatal("status/relay.json not written after the breaker closed")
	}
	if s := decodeStatus(t, raw); s.Storage == nil || s.Storage.State != "closed" {
		t.Fatalf("storage = %+v", s.Storage)
	}
	// The open interval continues into the running hour and is kept in memory.
	if got := r.cov.TakeHour(utc(15, 0, 0)); len(got.Ready) != 1 || !got.Ready[0][0].Equal(utc(15, 0, 0)) {
		t.Fatalf("running-hour coverage = %+v", got)
	}
}

func TestFailedCoverageWriteIsRetriedOnTheNextTick(t *testing.T) {
	r := newRig(t, utc(14, 0, 0))
	r.cov.SetReady(true, utc(14, 0, 0))
	stop := r.run()
	defer stop()
	r.store.waitPut(t, sidecar.StatusKey)
	r.clk.BlockUntil(1)

	r.store.setFail(2) // the status write and the coverage write of the next tick
	r.step(time.Hour)
	if _, ok := r.store.get(sidecar.CoverageKey("front-door", utc(14, 0, 0))); ok {
		t.Fatal("coverage present although the write failed")
	}
	r.step(60 * time.Second)
	raw, ok := r.store.get(sidecar.CoverageKey("front-door", utc(14, 0, 0)))
	if !ok {
		t.Fatal("coverage not retried on the next tick")
	}
	var c sidecar.Coverage
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	wantIvs(t, c, [2]time.Time{utc(14, 0, 0), utc(15, 0, 0)})
}

func TestFlushWritesPartialHour(t *testing.T) {
	r := newRig(t, utc(14, 0, 0))
	r.cov.SetReady(true, utc(14, 5, 0))
	r.cov.Drop(utc(14, 12, 0), "shutdown", 3000)
	r.clk.Advance(20 * time.Minute) // 14:20, publisher never ran

	r.pub.Flush(context.Background())

	key := sidecar.CoverageKey("front-door", utc(14, 0, 0))
	raw, ok := r.store.get(key)
	if !ok {
		t.Fatalf("partial hour not written; have %v", r.store.keys())
	}
	var c sidecar.Coverage
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("invalid: %v", err)
	}
	wantIvs(t, c, [2]time.Time{utc(14, 5, 0), utc(14, 20, 0)}) // ends at now, not the hour end
	if len(c.Dropped) != 1 || c.Dropped[0].Reason != "shutdown" {
		t.Fatalf("dropped = %+v", c.Dropped)
	}
}

func TestFlushSkipsWhileBreakerOpen(t *testing.T) {
	r := newRig(t, utc(14, 0, 0))
	r.cov.SetReady(true, utc(14, 5, 0))
	r.openBreaker()
	r.clk.Advance(20 * time.Minute)

	r.pub.Flush(context.Background())

	if keys := r.store.keys(); len(keys) != 0 {
		t.Fatalf("Flush wrote %v while the breaker was open", keys)
	}
}

// decodeCoverage reads and validates the stored coverage object of one hour.
func decodeCoverage(t *testing.T, store *memStore, hour time.Time) sidecar.Coverage {
	t.Helper()
	raw, ok := store.get(sidecar.CoverageKey("front-door", hour))
	if !ok {
		t.Fatalf("coverage for %v missing; have %v", hour, store.keys())
	}
	var c sidecar.Coverage
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("coverage JSON: %v\n%s", err, raw)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("coverage invalid: %v\n%s", err, raw)
	}
	return c
}

// secondRelay is a restarted process on the same store: new clock, tracker
// and publisher.
func secondRelay(store *memStore, start time.Time) (*clock.Fake, *status.CoverageTracker, *status.Publisher) {
	clk := clock.NewFake(start)
	cov := status.NewCoverageTracker(clk)
	pub := status.New(status.Config{
		Storage:  store,
		Reader:   store,
		RelayID:  "home-mini",
		Version:  "2.0.0",
		KeyID:    keyID,
		Coverage: map[string]*status.CoverageTracker{"front-door": cov},
		Clock:    clk,
		Interval: 60 * time.Second,
	})
	return clk, cov, pub
}

func TestRestartMidHourKeepsPartialCoverageAndDrops(t *testing.T) {
	// Relay A: ready from 14:05, a shutdown drop at 14:12, flushed at 14:20.
	a := newRig(t, utc(14, 0, 0))
	a.cov.SetReady(true, utc(14, 5, 0))
	a.cov.Drop(utc(14, 12, 0), "shutdown", 3000)
	a.clk.Advance(20 * time.Minute)
	a.pub.Flush(context.Background())
	wantIvs(t, decodeCoverage(t, a.store, utc(14, 0, 0)), [2]time.Time{utc(14, 5, 0), utc(14, 20, 0)})

	// Relay B starts at 14:22 on the same store and closes the hour at 15:02.
	clk, cov, pub := secondRelay(a.store, utc(14, 22, 0))
	cov.SetReady(true, utc(14, 23, 0))
	cov.Drop(utc(14, 40, 0), "backlog", 9000)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pub.Run(ctx) }()
	defer func() { cancel(); <-done }()
	a.store.waitPut(t, sidecar.StatusKey)
	clk.BlockUntil(1)
	clk.Advance(40 * time.Minute) // 15:02
	clk.BlockUntil(1)

	c := decodeCoverage(t, a.store, utc(14, 0, 0))
	wantIvs(t, c,
		[2]time.Time{utc(14, 5, 0), utc(14, 20, 0)},
		[2]time.Time{utc(14, 23, 0), utc(15, 0, 0)})
	if len(c.Dropped) != 2 || c.Dropped[0].Reason != "shutdown" || c.Dropped[1].Reason != "backlog" ||
		!c.Dropped[0].At.Equal(utc(14, 12, 0)) || !c.Dropped[1].At.Equal(utc(14, 40, 0)) {
		t.Fatalf("dropped = %+v, want the shutdown drop then the backlog drop", c.Dropped)
	}
}

func TestMergeKeepsTouchingIntervalsAndIsStableAcrossFlushes(t *testing.T) {
	r := newRig(t, utc(14, 0, 0))
	r.cov.SetReady(true, utc(14, 5, 0))
	r.cov.Drop(utc(14, 12, 0), "shutdown", 3000)
	r.clk.Advance(20 * time.Minute)
	r.pub.Flush(context.Background())

	r.clk.Advance(10 * time.Minute) // 14:30, same process flushes again
	r.pub.Flush(context.Background())
	c := decodeCoverage(t, r.store, utc(14, 0, 0))
	wantIvs(t, c, [2]time.Time{utc(14, 5, 0), utc(14, 30, 0)})
	if len(c.Dropped) != 1 {
		t.Fatalf("dropped = %+v, want the single shutdown drop", c.Dropped)
	}
}

func TestUnreadableStoredCoverageIsNotOverwritten(t *testing.T) {
	r := newRig(t, utc(14, 0, 0))
	r.cov.SetReady(true, utc(14, 5, 0))
	r.clk.Advance(20 * time.Minute)
	r.pub.Flush(context.Background())

	// The stored object cannot be read: writing now would lose it.
	r.store.mu.Lock()
	r.store.readErr = errors.New("read boom")
	r.store.mu.Unlock()
	r.clk.Advance(10 * time.Minute)
	r.pub.Flush(context.Background())
	wantIvs(t, decodeCoverage(t, r.store, utc(14, 0, 0)), [2]time.Time{utc(14, 5, 0), utc(14, 20, 0)})

	// Once readable, the retried write merges.
	r.store.mu.Lock()
	r.store.readErr = nil
	r.store.mu.Unlock()
	r.pub.Flush(context.Background())
	wantIvs(t, decodeCoverage(t, r.store, utc(14, 0, 0)), [2]time.Time{utc(14, 5, 0), utc(14, 30, 0)})
}

func TestCorruptStoredCoverageIsReplaced(t *testing.T) {
	r := newRig(t, utc(14, 0, 0))
	key := sidecar.CoverageKey("front-door", utc(14, 0, 0))
	r.store.mu.Lock()
	r.store.objects[key] = []byte("{not json")
	r.store.mu.Unlock()
	r.cov.SetReady(true, utc(14, 5, 0))
	r.clk.Advance(20 * time.Minute)
	r.pub.Flush(context.Background())
	wantIvs(t, decodeCoverage(t, r.store, utc(14, 0, 0)), [2]time.Time{utc(14, 5, 0), utc(14, 20, 0)})
}

func TestIdleRelayWithStorageDownOpensTheBreaker(t *testing.T) {
	r := newRigThreshold(t, utc(3, 0, 0), 1) // an idle night: no clip, so no drain worker reports
	r.store.setFail(1000)
	stop := r.run()
	defer stop()
	r.clk.BlockUntil(1) // first tick done; its status write failed (threshold is 1)

	if s, _ := r.brk.State(); s != egress.Open {
		t.Fatalf("breaker = %v after a failed status write, want Open", s)
	}
}

func TestPublisherPermanentErrorOpensBreaker(t *testing.T) {
	start := utc(3, 0, 0)
	clk := clock.NewFake(start)
	brk := egress.NewBreaker(egress.BreakerConfig{FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Second},
		clock.NewFake(start), func(context.Context) error { return nil }, nil)
	store := newMemStore()
	store.setFail(1)
	pub := status.New(status.Config{
		Storage:  store,
		Breaker:  brk,
		Classify: func(error) storage.ErrorClass { return storage.Permanent },
		RelayID:  "home-mini",
		Version:  "2.0.0",
		Coverage: map[string]*status.CoverageTracker{},
		Clock:    clk,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pub.Run(ctx) }()
	clk.BlockUntil(1)
	cancel()
	<-done
	if s, _ := brk.State(); s != egress.Open {
		t.Fatalf("breaker = %v after one permanent write error with threshold 5, want Open", s)
	}
}

func TestSuccessfulWritesKeepTheBreakerClosed(t *testing.T) {
	r := newRig(t, utc(3, 0, 0))
	stop := r.run()
	defer stop()
	r.store.waitPut(t, sidecar.StatusKey)
	r.clk.BlockUntil(1)
	if s, _ := r.brk.State(); s != egress.Closed {
		t.Fatalf("breaker = %v, want Closed", s)
	}
}
