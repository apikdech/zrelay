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
	defer s.mu.Unlock()
	s.record("put", key)
	s.small[key] = bytes.Clone(data)
	return nil
}

func (s *fakeStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("delete", key)
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
	o.data = o.data[:o.durable] // the driver repairs its own position
	return int64(o.durable), nil
}

func (o *fakeObj) Commit(ctx context.Context) error {
	s := o.s
	s.mu.Lock()
	gate := s.commitGate
	s.mu.Unlock()
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
	clk := clock.NewFake(time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC))
	r := &egressRig{t: t, store: store, clk: clk, runDone: make(chan struct{})}
	r.br = NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Minute}, clk,
		func(ctx context.Context) error { return nil }, nil)
	r.e = New(Config{
		Storage:      store,
		Workers:      workers,
		BacklogLimit: limit,
		Breaker:      r.br,
		Clock:        clk,
		Classify:     func(error) storage.ErrorClass { return storage.Transient },
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
