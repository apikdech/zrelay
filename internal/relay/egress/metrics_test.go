package egress

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

// countingMetrics is a UploadMetrics that records what it is told.
type countingMetrics struct {
	mu      sync.Mutex
	bytes   int64
	errors  map[string]int
	resumes int
}

func (c *countingMetrics) UploadBytes(n int64) {
	c.mu.Lock()
	c.bytes += n
	c.mu.Unlock()
}

func (c *countingMetrics) UploadError(kind string) {
	c.mu.Lock()
	if c.errors == nil {
		c.errors = map[string]int{}
	}
	c.errors[kind]++
	c.mu.Unlock()
}

func (c *countingMetrics) UploadResume() {
	c.mu.Lock()
	c.resumes++
	c.mu.Unlock()
}

func (c *countingMetrics) snap() (bytes int64, errs map[string]int, resumes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	errs = map[string]int{}
	for k, v := range c.errors {
		errs[k] = v
	}
	return c.bytes, errs, c.resumes
}

// newMetricsRig is an Egress with an UploadMetrics hook on a fake clock.
func newMetricsRig(t *testing.T, store *fakeStore, m UploadMetrics) *egressRig {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC))
	r := &egressRig{t: t, store: store, clk: clk, runDone: make(chan struct{})}
	r.br = NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Minute}, clk,
		func(ctx context.Context) error { return nil }, nil)
	r.e = New(Config{
		Storage: store, Workers: 1, BacklogLimit: 1 << 30, Breaker: r.br, Clock: clk,
		Classify: classify, Metrics: m,
	})
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.e.Run(ctx); close(r.runDone) }()
	t.Cleanup(func() { cancel(); <-r.runDone })
	return r
}

// A healthy upload counts every ciphertext byte once, including the final
// part that only Commit sends, and reports no error or resume.
func TestUploadMetricsHealthyUploadCountsEveryByteOnce(t *testing.T) {
	st := newFakeStore()
	st.partSize = 4 * copyBufSize // buffering like the S3 driver
	m := &countingMetrics{}
	r := newMetricsRig(t, st, m)
	c := r.begin("front", "A")
	payload := randBytes(10*copyBufSize+123, 31)
	c.write(t, payload)
	c.finish()
	eventually(t, "commit", func() bool { return st.count("commit") == 1 })
	eventually(t, "final sidecar", func() bool { return r.e.BacklogBytes() == 0 })

	bytes, errs, resumes := m.snap()
	if bytes != int64(len(payload)) {
		t.Errorf("UploadBytes total = %d, want %d", bytes, len(payload))
	}
	if len(errs) != 0 || resumes != 0 {
		t.Errorf("errors=%v resumes=%d, want none", errs, resumes)
	}
}

// A failed Write is an upload error of its class, the Resume that follows is
// counted, and the bytes written again after the resume are not counted twice.
func TestUploadMetricsFailedWriteIsAnErrorAndResumeIsCounted(t *testing.T) {
	st := newFakeStore()
	st.failFirstWrite = true
	st.durableStep = 4096
	m := &countingMetrics{}
	r := newMetricsRig(t, st, m)
	c := r.begin("front", "A")
	payload := randBytes(300_000, 32)
	for off := 0; off < len(payload); off += 50_000 {
		c.write(t, payload[off:off+50_000])
	}
	c.finish()
	eventually(t, "commit", func() bool { return st.count("commit") == 1 })
	eventually(t, "backlog released", func() bool { return r.e.BacklogBytes() == 0 })

	bytes, errs, resumes := m.snap()
	if errs["transient"] != 1 {
		t.Errorf("errors = %v, want one transient", errs)
	}
	if resumes != 1 {
		t.Errorf("resumes = %d, want 1", resumes)
	}
	if bytes != int64(len(payload)) {
		t.Errorf("UploadBytes total = %d, want %d (no double counting after a resume)", bytes, len(payload))
	}
}

// A request that fails all of its per-request attempts is one upload error
// (the final outcome, as the breaker sees it).
func TestUploadMetricsRequestFailureIsAnError(t *testing.T) {
	st := newFakeStore()
	st.putFails = 3 // every attempt of the first staging PutSmall
	m := &countingMetrics{}
	r := newMetricsRig(t, st, m)
	c := r.begin("front", "A")
	c.write(t, randBytes(1000, 33))
	c.finish()
	// The fake clock drives the retry backoff.
	advanceUntil(t, r.clk, "commit", func() bool { return st.count("commit") == 1 })
	_, errs, _ := m.snap()
	if errs["transient"] != 1 {
		t.Errorf("errors = %v, want exactly one transient error for the failed request", errs)
	}
}
