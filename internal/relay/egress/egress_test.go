package egress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/recorder"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

var _ recorder.ClipSink = (*Egress)(nil)

// goid returns the id of the calling goroutine.
func goid() uint64 {
	var b [64]byte
	s := strings.TrimPrefix(string(b[:runtime.Stack(b[:], false)]), "goroutine ")
	id, _ := strconv.ParseUint(s[:strings.IndexByte(s, ' ')], 10, 64)
	return id
}

// fakeStore is a storage.Writer with scriptable failures. Every call is
// recorded in events as "<op>:<key>".
type fakeStore struct {
	mu      sync.Mutex
	events  []string
	small   map[string][]byte
	objects map[string][]byte
	objs    map[string]*fakeObj

	// durableStep makes DurableOffset lag: only whole multiples of it count
	// until Commit. 0 means every accepted byte is durable.
	durableStep int
	// failFirstWrite makes the first Write call accept half the bytes and fail.
	failFirstWrite bool
	// down makes every Write fail (an outage).
	down bool
	// commitGate, when non-nil, blocks Commit until closed (or ctx ends).
	commitGate chan struct{}
	// finalGate, when non-nil, blocks PutSmall of a final (non-staging)
	// sidecar until closed (or ctx ends).
	finalGate chan struct{}
	// putFails makes the next N PutSmall calls fail with errTransient.
	putFails int
	// deleteErr, when set, is returned by every Delete (after it is recorded).
	deleteErr error
	// resumeErrs are returned, one per Resume call, before Resume succeeds.
	resumeErrs []error
	// commitErrs are returned, one per Commit call, before Commit succeeds.
	commitErrs []error

	writeCalls, writeErrs, resumes int
}

func newFakeStore() *fakeStore {
	return &fakeStore{small: map[string][]byte{}, objects: map[string][]byte{}, objs: map[string]*fakeObj{}}
}

func (s *fakeStore) record(op, key string) {
	s.events = append(s.events, op+":"+key)
}

func (s *fakeStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

func (s *fakeStore) count(op string) int {
	n := 0
	for _, e := range s.snapshot() {
		if strings.HasPrefix(e, op+":") {
			n++
		}
	}
	return n
}

func (s *fakeStore) PutSmall(ctx context.Context, key string, data []byte, contentType string) error {
	s.mu.Lock()
	if gate := s.finalGate; gate != nil && !strings.Contains(key, "/.") {
		s.mu.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if s.putFails > 0 {
		s.putFails--
		return errTransient
	}
	s.record("put", key)
	s.small[key] = bytes.Clone(data)
	return nil
}

func (s *fakeStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("delete", key)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.small, key)
	return nil
}

func (s *fakeStore) Create(ctx context.Context, key string) (storage.ObjectWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("create", key)
	o := &fakeObj{s: s, key: key, createGoid: goid()}
	s.objs[key] = o
	return o, nil
}

type fakeObj struct {
	s          *fakeStore
	key        string
	data       []byte
	durable    int
	createGoid uint64
	abortGoid  uint64
	aborted    bool
}

func (o *fakeObj) Write(p []byte) (int, error) {
	s := o.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeCalls++
	if s.down {
		s.writeErrs++
		return 0, errors.New("storage down")
	}
	if s.failFirstWrite && s.writeErrs == 0 {
		s.writeErrs++
		half := len(p) / 2
		o.data = append(o.data, p[:half]...) // accepted but not durable
		return half, errors.New("connection reset")
	}
	o.data = append(o.data, p...)
	if s.durableStep > 0 {
		o.durable = len(o.data) - len(o.data)%s.durableStep
	} else {
		o.durable = len(o.data)
	}
	return len(p), nil
}

func (o *fakeObj) DurableOffset() int64 {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	return int64(o.durable)
}

func (o *fakeObj) Resume(ctx context.Context) (int64, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	o.s.resumes++
	if len(o.s.resumeErrs) > 0 {
		err := o.s.resumeErrs[0]
		o.s.resumeErrs = o.s.resumeErrs[1:]
		return 0, err
	}
	o.data = o.data[:o.durable] // the driver repairs its own position
	return int64(o.durable), nil
}

func (o *fakeObj) Commit(ctx context.Context) error {
	s := o.s
	s.mu.Lock()
	gate := s.commitGate
	var cerr error
	if len(s.commitErrs) > 0 {
		cerr, s.commitErrs = s.commitErrs[0], s.commitErrs[1:]
	}
	s.mu.Unlock()
	if cerr != nil {
		return cerr
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("commit", o.key)
	o.durable = len(o.data)
	s.objects[o.key] = bytes.Clone(o.data)
	return nil
}

func (o *fakeObj) Abort(ctx context.Context) error {
	s := o.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("abort", o.key)
	o.aborted = true
	o.abortGoid = goid()
	return nil
}

func (s *fakeStore) obj(key string) *fakeObj {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objs[key]
}

type drop struct {
	camera, reason string
	ms             int64
}

type egressRig struct {
	t     *testing.T
	e     *Egress
	store *fakeStore
	clk   *clock.Fake
	br    *Breaker

	mu        sync.Mutex
	drops     []drop
	committed []string
	forced    []string
	cancel    context.CancelFunc
	runDone   chan struct{}
}

func (r *egressRig) dropList() []drop {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.drops)
}

// newRig starts an Egress with a real Breaker on a fake clock. The breaker's
// own recovery loop is started only by startBreaker.
func newEgressRig(t *testing.T, workers int, limit int64, store *fakeStore) *egressRig {
	t.Helper()
	return newEgressRigOpts(t, workers, limit, store, true)
}

// newEgressRigOpts is newEgressRig with the breaker optional: without one the
// Egress falls back to its fixed retry delay.
func newEgressRigOpts(t *testing.T, workers int, limit int64, store *fakeStore, withBreaker bool) *egressRig {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC))
	r := &egressRig{t: t, store: store, clk: clk, runDone: make(chan struct{})}
	var br *Breaker
	if withBreaker {
		br = NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Minute}, clk,
			func(ctx context.Context) error { return nil }, nil)
		r.br = br
	}
	r.e = New(Config{
		Storage:      store,
		Workers:      workers,
		BacklogLimit: limit,
		Breaker:      br,
		Clock:        clk,
		Classify:     classify, // retry_test.go: errMissing is NotFound, errPermanent Permanent
		OnDrop: func(camera, reason string, ms int64) {
			r.mu.Lock()
			r.drops = append(r.drops, drop{camera, reason, ms})
			r.mu.Unlock()
		},
		OnCommitted: func(camera string, at time.Time) {
			r.mu.Lock()
			r.committed = append(r.committed, camera)
			r.mu.Unlock()
		},
		ForceFinalize: func(camera string) {
			r.mu.Lock()
			r.forced = append(r.forced, camera)
			r.mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.e.Run(ctx); close(r.runDone) }()
	t.Cleanup(func() { cancel(); <-r.runDone })
	return r
}

func (r *egressRig) startBreaker() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.br.Run(ctx); close(done) }()
	r.t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

type testClip struct {
	pc *recorder.PendingClip
	w  io.Writer
}

func (r *egressRig) begin(camera, id string) *testClip {
	r.t.Helper()
	return r.beginWith(camera, id, nil)
}

// beginWith is begin with a hook that may adjust the PendingClip first.
func (r *egressRig) beginWith(camera, id string, tweak func(*recorder.PendingClip)) *testClip {
	r.t.Helper()
	dir := "clips/" + camera + "/2026/11/06"
	pc := &recorder.PendingClip{
		Keys: sidecar.ClipKeySet{
			Dir:            dir,
			Video:          dir + "/145900000_" + id + ".zrec",
			Sidecar:        dir + "/145900000_" + id + ".json",
			StagingSidecar: dir + "/.145900000_" + id + ".json.part",
		},
		Staging: []byte(`{"staging":"` + id + `"}`),
		Final:   func() ([]byte, error) { return []byte(`{"final":"` + id + `"}`), nil },
	}
	if tweak != nil {
		tweak(pc)
	}
	w, err := r.e.Begin(pc)
	if err != nil {
		r.t.Fatalf("Begin: %v", err)
	}
	return &testClip{pc: pc, w: w}
}

func (c *testClip) write(t *testing.T, p []byte) {
	t.Helper()
	if _, err := c.w.Write(p); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func (c *testClip) finish() { c.pc.Done(nil) }

func randBytes(n int, seed uint64) []byte {
	rng := rand.New(rand.NewPCG(seed, 1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func TestCommitOrdering(t *testing.T) {
	st := newFakeStore()
	r := newEgressRig(t, 2, 1<<30, st)
	c := r.begin("front", "A")
	payload := randBytes(100_000, 1)
	c.write(t, payload[:40_000])
	c.write(t, payload[40_000:])
	c.finish()

	eventually(t, "staging delete", func() bool { return st.count("delete") == 1 })

	var got []string
	for _, e := range st.snapshot() {
		if !strings.HasPrefix(e, "create:") {
			got = append(got, e)
		}
	}
	k := c.pc.Keys
	want := []string{"put:" + k.StagingSidecar, "commit:" + k.Video, "put:" + k.Sidecar, "delete:" + k.StagingSidecar}
	if !slices.Equal(got, want) {
		t.Fatalf("event order\n got %v\nwant %v", got, want)
	}
	st.mu.Lock()
	obj, final, staging := st.objects[k.Video], st.small[k.Sidecar], st.small[k.StagingSidecar]
	st.mu.Unlock()
	if !bytes.Equal(obj, payload) {
		t.Fatalf("committed object differs from the stream (%d vs %d bytes)", len(obj), len(payload))
	}
	if string(final) != `{"final":"A"}` {
		t.Fatalf("final sidecar = %q", final)
	}
	if len(staging) != 0 {
		t.Fatal("staging sidecar still present")
	}
	eventually(t, "OnCommitted", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.committed) == 1 && r.committed[0] == "front"
	})
	eventually(t, "backlog released", func() bool { return r.e.BacklogBytes() == 0 })
}

func TestResumeAfterTransientWriteError(t *testing.T) {
	st := newFakeStore()
	st.failFirstWrite = true
	st.durableStep = 4096 // DurableOffset lags the accepted bytes
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	payload := randBytes(300_000, 2)
	for off := 0; off < len(payload); off += 50_000 {
		c.write(t, payload[off:off+50_000])
	}
	c.finish()

	eventually(t, "commit", func() bool { return st.count("commit") == 1 })
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.resumes == 0 || st.writeErrs != 1 {
		t.Fatalf("resumes=%d writeErrs=%d, want a resume after the one failed write", st.resumes, st.writeErrs)
	}
	if !bytes.Equal(st.objects[c.pc.Keys.Video], payload) {
		t.Fatalf("object bytes differ from the stream (%d vs %d bytes)", len(st.objects[c.pc.Keys.Video]), len(payload))
	}
}

func TestBreakerOpenParksWorkersAndBacklogGrows(t *testing.T) {
	st := newFakeStore()
	r := newEgressRig(t, 2, 1<<30, st)
	r.br.Failure(true) // permanent failure opens the breaker
	if s, _ := r.br.State(); s != Open {
		t.Fatalf("breaker state = %v, want Open", s)
	}
	c := r.begin("front", "A")
	c.write(t, make([]byte, 10_000))
	c.write(t, make([]byte, 5_000))
	if got := r.e.BacklogBytes(); got != 15_000 {
		t.Fatalf("BacklogBytes = %d, want 15000", got)
	}
	time.Sleep(30 * time.Millisecond) // give a wrongly unparked worker time to act
	if ev := st.snapshot(); len(ev) != 0 {
		t.Fatalf("workers sent requests while the breaker was open: %v", ev)
	}
	c.write(t, make([]byte, 1_000))
	if got := r.e.BacklogBytes(); got != 16_000 {
		t.Fatalf("BacklogBytes = %d, want 16000", got)
	}
}

func TestBreakerOpenParksWorkersAndBacklogDrainsAfterClose(t *testing.T) {
	st := newFakeStore()
	r := newEgressRig(t, 2, 1<<30, st)
	r.br.Failure(true)
	r.startBreaker()
	c := r.begin("front", "A")
	payload := randBytes(20_000, 3)
	c.write(t, payload)
	c.finish()
	if got := r.e.BacklogBytes(); got != 20_000 {
		t.Fatalf("BacklogBytes = %d, want 20000 while parked", got)
	}

	r.clk.BlockUntil(1) // the breaker's backoff timer
	r.clk.Advance(2 * time.Second)

	eventually(t, "commit after the breaker closed", func() bool { return st.count("commit") == 1 })
	eventually(t, "backlog drained", func() bool { return r.e.BacklogBytes() == 0 })
	st.mu.Lock()
	defer st.mu.Unlock()
	if !bytes.Equal(st.objects[c.pc.Keys.Video], payload) {
		t.Fatal("object differs from the stream")
	}
}

func TestEvictionAbortsByOwner(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30 // nothing becomes durable, so bytes stay in the ring
	st.commitGate = make(chan struct{})
	r := newEgressRig(t, 2, 1000, st)

	a := r.begin("front", "A")
	a.write(t, make([]byte, 800))
	a.finish()
	r.clk.Advance(time.Second) // B is younger than A
	b := r.begin("back", "B")
	eventually(t, "A created", func() bool { return st.obj(a.pc.Keys.Video) != nil })

	b.write(t, make([]byte, 500)) // 1300 > 1000: evicts A, the oldest finalized clip

	eventually(t, "abort of A", func() bool { return st.count("abort") == 1 })
	oa := st.obj(a.pc.Keys.Video)
	st.mu.Lock()
	createGoid, abortGoid := oa.createGoid, oa.abortGoid
	st.mu.Unlock()
	if abortGoid != createGoid {
		t.Fatalf("Abort ran on goroutine %d, the owning worker is %d", abortGoid, createGoid)
	}
	if me := goid(); abortGoid == me {
		t.Fatal("Abort ran on the goroutine that triggered the eviction")
	}
	eventually(t, "OnDrop", func() bool { return len(r.dropList()) == 1 })
	d := r.dropList()[0]
	if d.camera != "front" || d.reason != "backlog" || d.ms != 1000 {
		t.Fatalf("drop = %+v, want camera front, reason backlog, 1000 ms", d)
	}
	if st.count("commit") != 0 {
		t.Fatal("evicted clip was committed")
	}
	if _, err := a.w.Write([]byte{1}); !errors.Is(err, ErrEvicted) {
		t.Fatalf("Write after eviction = %v, want ErrEvicted", err)
	}
	close(st.commitGate)
}

func TestWorkersLimit(t *testing.T) {
	st := newFakeStore()
	r := newEgressRig(t, 2, 1<<30, st)
	a := r.begin("c1", "A")
	b := r.begin("c2", "B")
	c := r.begin("c3", "C")
	a.write(t, []byte("x"))
	b.write(t, []byte("x"))
	c.write(t, []byte("x"))

	eventually(t, "two creates", func() bool { return st.count("create") == 2 })
	time.Sleep(30 * time.Millisecond)
	if n := st.count("create"); n != 2 {
		t.Fatalf("creates = %d with 2 workers, want the third clip to wait", n)
	}
	if st.obj(c.pc.Keys.Video) != nil || slices.Contains(st.snapshot(), "put:"+c.pc.Keys.StagingSidecar) {
		t.Fatal("third clip started before a worker was free")
	}
	// The two running clips drain their byte; the queued clip's byte stays
	// counted against the backlog.
	eventually(t, "only the queued clip's byte buffered", func() bool { return r.e.BacklogBytes() == 1 })

	a.finish() // frees a worker
	eventually(t, "third clip created", func() bool { return st.obj(c.pc.Keys.Video) != nil })
	b.finish()
	c.finish()
	eventually(t, "all committed", func() bool { return st.count("commit") == 3 })
}

func TestShutdownWithOpenBreakerExitsWithinGrace(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30
	r := newEgressRig(t, 2, 1<<30, st)
	a := r.begin("front", "A")
	b := r.begin("back", "B")
	a.write(t, make([]byte, 100))
	b.write(t, make([]byte, 100))
	eventually(t, "both created", func() bool { return st.count("create") == 2 })

	// Storage goes down: writes fail, the breaker opens, workers park.
	st.mu.Lock()
	st.down = true
	st.mu.Unlock()
	r.br.Failure(true)
	a.write(t, make([]byte, 100))
	b.write(t, make([]byte, 100))
	eventually(t, "both workers saw the outage", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.writeErrs >= 2
	})
	if got := r.e.BacklogBytes(); got != 400 {
		t.Fatalf("BacklogBytes = %d, want 400", got)
	}

	res := make(chan int, 1)
	go func() { res <- r.e.Shutdown(30 * time.Second) }()
	r.clk.BlockUntil(1) // Shutdown's grace timer
	r.clk.Advance(29 * time.Second)
	select {
	case n := <-res:
		t.Fatalf("Shutdown returned %d before the grace period elapsed", n)
	case <-time.After(20 * time.Millisecond):
	}
	r.clk.Advance(time.Second)

	select {
	case n := <-res:
		if n != 2 {
			t.Fatalf("Shutdown dropped %d clips, want 2", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the grace period")
	}
	if st.count("abort") != 2 {
		t.Fatalf("aborts = %d, want 2", st.count("abort"))
	}
	if st.count("commit") != 0 {
		t.Fatal("a clip was committed during an outage")
	}
	ds := r.dropList()
	if len(ds) != 2 {
		t.Fatalf("drops = %+v, want 2", ds)
	}
	cams := []string{ds[0].camera, ds[1].camera}
	slices.Sort(cams)
	if ds[0].reason != "shutdown" || ds[1].reason != "shutdown" || cams[0] != "back" || cams[1] != "front" {
		t.Fatalf("drops = %+v, want shutdown for front and back", ds)
	}
	if _, err := r.e.Begin(&recorder.PendingClip{Keys: a.pc.Keys}); err == nil {
		t.Fatal("Begin succeeded after Shutdown")
	}
}

func TestShutdownDrainsHealthyBacklogWithoutDrops(t *testing.T) {
	st := newFakeStore()
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	c.write(t, randBytes(10_000, 4))
	c.finish()
	res := make(chan int, 1)
	go func() { res <- r.e.Shutdown(30 * time.Second) }()
	select {
	case n := <-res:
		if n != 0 {
			t.Fatalf("Shutdown dropped %d, want 0", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return although the backlog drained")
	}
	if st.count("commit") != 1 || st.count("delete") != 1 {
		t.Fatalf("events = %v, want the clip fully committed", st.snapshot())
	}
}

func TestRecorderAbortAbandonsWithoutDrop(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	c.write(t, make([]byte, 100))
	eventually(t, "created", func() bool { return st.obj(c.pc.Keys.Video) != nil })

	c.pc.Done(fmt.Errorf("clip aborted"))

	eventually(t, "abort", func() bool { return st.count("abort") == 1 })
	eventually(t, "staging deleted", func() bool { return st.count("delete") == 1 })
	if st.count("commit") != 0 || len(r.dropList()) != 0 {
		t.Fatalf("abandoned clip: commits=%d drops=%v", st.count("commit"), r.dropList())
	}
}

func TestForceFinalizeIsForwarded(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30
	r := newEgressRig(t, 1, 100, st)
	c := r.begin("front", "A")
	c.write(t, make([]byte, 200)) // over the limit, nothing finalized yet
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Equal(r.forced, []string{"front"}) {
		t.Fatalf("ForceFinalize calls = %v, want [front]", r.forced)
	}
}

// advanceUntil advances the fake clock in small steps until cond holds, so
// workers sleeping on the fake clock make progress.
func advanceUntil(t *testing.T, clk *clock.Fake, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		clk.Advance(500 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

func (s *fakeStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCalls
}

// A storage outage that fails every Write, while Resume (local in the S3
// driver) keeps succeeding, must still open the breaker after
// FailureThreshold failures, back off between writes, park the worker and let
// Run return on cancel. No manual Breaker.Failure call is involved.
func TestWriteFailuresOpenBreakerAndParkWorker(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	eventually(t, "created", func() bool { return st.obj(c.pc.Keys.Video) != nil })
	st.mu.Lock()
	st.down = true
	st.mu.Unlock()
	c.write(t, make([]byte, 100))

	advanceUntil(t, r.clk, "breaker to open from write failures", func() bool {
		s, _ := r.br.State()
		return s == Open
	})
	// The worker is parked: no further writes however long we wait.
	time.Sleep(30 * time.Millisecond)
	parked := st.writes()
	if parked != 5 {
		t.Fatalf("writes before the breaker opened = %d, want FailureThreshold (5)", parked)
	}
	for range 20 {
		r.clk.Advance(2 * time.Second)
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if got := st.writes(); got != parked {
		t.Fatalf("worker kept writing while the breaker was open: %d -> %d", parked, got)
	}
	if got := r.e.BacklogBytes(); got != 100 {
		t.Fatalf("BacklogBytes = %d, want the 100 bytes to stay buffered", got)
	}

	r.cancel()
	select {
	case <-r.runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// Consecutive write failures are paced by backoff on the clock, not retried in
// a tight loop.
func TestWriteFailuresBackOff(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	eventually(t, "created", func() bool { return st.obj(c.pc.Keys.Video) != nil })
	st.mu.Lock()
	st.down = true
	st.mu.Unlock()
	c.write(t, make([]byte, 100))
	eventually(t, "first failed write", func() bool { return st.writes() >= 1 })
	time.Sleep(50 * time.Millisecond) // without backoff this spins thousands of times
	if got := st.writes(); got > 2 {
		t.Fatalf("%d writes in 50 ms of real time without the clock advancing: no backoff", got)
	}
}

// A committed clip is no longer backlog: while its final sidecar is blocked,
// another clip overflowing the limit must not evict it.
func TestCommittedClipSurvivesBacklogOverflow(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 4096 // the tail below one step stays buffered until Commit
	st.finalGate = make(chan struct{})
	r := newEgressRig(t, 2, 1000, st)
	a := r.begin("front", "A")
	a.write(t, make([]byte, 800))
	a.finish()
	eventually(t, "A committed", func() bool { return st.count("commit") == 1 })
	r.clk.Advance(time.Second)

	b := r.begin("back", "B")
	b.write(t, make([]byte, 500)) // 500 <= 1000 once A no longer counts
	time.Sleep(30 * time.Millisecond)
	if ds := r.dropList(); len(ds) != 0 {
		t.Fatalf("drops = %+v, want none: A is already committed", ds)
	}
	if st.count("abort") != 0 {
		t.Fatalf("aborts = %d, want 0", st.count("abort"))
	}

	close(st.finalGate)
	k := a.pc.Keys
	eventually(t, "A's final sidecar", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return string(st.small[k.Sidecar]) == `{"final":"A"}`
	})
	eventually(t, "A's staging delete", func() bool { return st.count("delete") == 1 })
}

// The upload vanished (NotFound on Resume) while the stream still holds every
// byte: the clip is re-created and restarted, and the committed object equals
// the stream.
func TestUploadGoneRestartsFromStream(t *testing.T) {
	st := newFakeStore()
	st.failFirstWrite = true
	st.resumeErrs = []error{errMissing}
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	payload := randBytes(300_000, 7)
	for off := 0; off < len(payload); off += 50_000 {
		c.write(t, payload[off:off+50_000])
	}
	c.finish()

	advanceUntil(t, r.clk, "commit after restart", func() bool { return st.count("commit") == 1 })
	if st.count("create") != 2 || st.count("abort") != 1 {
		t.Fatalf("events = %v, want a re-Create and one Abort of the vanished upload", st.snapshot())
	}
	st.mu.Lock()
	got := st.objects[c.pc.Keys.Video]
	st.mu.Unlock()
	if !bytes.Equal(got, payload) {
		t.Fatalf("object differs from the stream (%d vs %d bytes)", len(got), len(payload))
	}
	if ds := r.dropList(); len(ds) != 0 {
		t.Fatalf("drops = %+v, want none", ds)
	}
}

// When the upload vanished and the bytes are no longer all held, the clip is
// abandoned: the writer is aborted, the staging sidecar removed, and no drop
// reason outside the coverage enum is reported.
func TestUploadGoneAbandonsWhenBytesReleased(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleteErr error
	}{
		{"staging deleted", nil},
		{"staging already gone", errMissing},
		{"staging delete fails", errTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.deleteErr = tc.deleteErr
			r := newEgressRig(t, 1, 1<<30, st)
			c := r.begin("front", "A")
			c.write(t, make([]byte, 1000))
			eventually(t, "bytes durable and released", func() bool {
				return st.obj(c.pc.Keys.Video) != nil && r.e.BacklogBytes() == 0
			})
			st.mu.Lock()
			st.down = true
			st.resumeErrs = []error{errMissing}
			st.mu.Unlock()
			c.write(t, make([]byte, 100))

			advanceUntil(t, r.clk, "abandon", func() bool { return st.count("abort") == 1 && st.count("delete") == 1 })
			eventually(t, "backlog freed", func() bool { return r.e.BacklogBytes() == 0 })
			if st.count("commit") != 0 {
				t.Fatal("abandoned clip was committed")
			}
			if ds := r.dropList(); len(ds) != 0 {
				t.Fatalf("drops = %+v, want none (no reason outside the coverage enum)", ds)
			}
		})
	}
}

// A clip is restarted at most once; a second vanished upload abandons it.
func TestUploadGoneTwiceAbandons(t *testing.T) {
	st := newFakeStore()
	st.down = true
	st.resumeErrs = []error{errMissing, errMissing}
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	c.write(t, make([]byte, 100))
	advanceUntil(t, r.clk, "both uploads aborted", func() bool { return st.count("abort") == 2 })
	if st.count("create") != 2 || st.count("commit") != 0 {
		t.Fatalf("events = %v", st.snapshot())
	}
	eventually(t, "backlog freed", func() bool { return r.e.BacklogBytes() == 0 })
	if ds := r.dropList(); len(ds) != 0 {
		t.Fatalf("drops = %+v, want none", ds)
	}
}

// Without a circuit breaker a failing request is retried after a fixed delay.
func TestPersistWithoutBreakerRetriesAfterDelay(t *testing.T) {
	st := newFakeStore()
	st.putFails = 4 // more than one Do's three attempts
	r := newEgressRigOpts(t, 1, 1<<30, st, false)
	c := r.begin("front", "A")
	c.write(t, []byte("payload"))
	c.finish()
	advanceUntil(t, r.clk, "commit", func() bool { return st.count("commit") == 1 })
	eventually(t, "staging delete", func() bool { return st.count("delete") == 1 })
	if st.count("put") < 2 {
		t.Fatalf("events = %v", st.snapshot())
	}
}

// If the final sidecar cannot be built the video stays committed and the
// staging sidecar is left for the recovery sweep.
func TestFinalSidecarErrorLeavesStagingForRecovery(t *testing.T) {
	st := newFakeStore()
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.beginWith("front", "A", func(pc *recorder.PendingClip) {
		pc.Final = func() ([]byte, error) { return nil, errors.New("finalize failed") }
	})
	c.write(t, []byte("payload"))
	c.finish()
	eventually(t, "commit", func() bool { return st.count("commit") == 1 })
	eventually(t, "job finished", func() bool {
		r.e.mu.Lock()
		defer r.e.mu.Unlock()
		return len(r.e.jobs) == 0
	})
	k := c.pc.Keys
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.small[k.Sidecar]; ok {
		t.Fatal("a final sidecar was stored")
	}
	if _, ok := st.small[k.StagingSidecar]; !ok {
		t.Fatal("the staging sidecar was removed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.committed) != 0 {
		t.Fatal("OnCommitted fired without a final sidecar")
	}
}

func TestCameraOf(t *testing.T) {
	for in, want := range map[string]string{
		"clips/front/2026/11/06": "front",
		"clips/front":            "front",
		"front":                  "front",
		"":                       "",
	} {
		if got := cameraOf(in); got != want {
			t.Errorf("cameraOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A Resume that fails transiently is retried (after backoff) and the upload
// continues from the offset the later Resume reports.
func TestResumeErrorIsRetried(t *testing.T) {
	st := newFakeStore()
	st.failFirstWrite = true
	st.resumeErrs = []error{errTransient, errTransient}
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	payload := randBytes(100_000, 9)
	c.write(t, payload)
	c.finish()
	advanceUntil(t, r.clk, "commit", func() bool { return st.count("commit") == 1 })
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.resumes != 3 {
		t.Fatalf("resumes = %d, want 3 (two failures, one success)", st.resumes)
	}
	if !bytes.Equal(st.objects[c.pc.Keys.Video], payload) {
		t.Fatal("object differs from the stream")
	}
}

// Commit answering NotFound (the multipart upload is gone) with every byte
// still in the stream restarts the clip rather than dropping it.
func TestCommitNotFoundRestartsFromStream(t *testing.T) {
	st := newFakeStore()
	st.durableStep = 1 << 30 // nothing durable before Commit: no byte is released
	st.commitErrs = []error{errMissing}
	r := newEgressRig(t, 1, 1<<30, st)
	c := r.begin("front", "A")
	payload := randBytes(100_000, 11)
	c.write(t, payload)
	c.finish()
	eventually(t, "commit after restart", func() bool { return st.count("commit") == 1 })
	if st.count("create") != 2 || st.count("abort") != 1 {
		t.Fatalf("events = %v, want a re-Create and one Abort", st.snapshot())
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !bytes.Equal(st.objects[c.pc.Keys.Video], payload) {
		t.Fatal("object differs from the stream")
	}
	if ds := r.dropList(); len(ds) != 0 {
		t.Fatalf("drops = %+v, want none", ds)
	}
}
