package zview

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/local"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const testRoot = "zrelay"

// testNow is the fake clock's start: 2026-11-06 12:00 UTC.
var testNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// fixture is a storage tree on the local driver holding real sidecars and
// small real zrec objects.
type fixture struct {
	t    *testing.T
	base string
	drv  *local.Driver
	clk  *clock.Fake
	seq  uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	drv, err := local.New(base, testRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, base: base, drv: drv, clk: clock.NewFake(testNow)}
}

// clipOpt describes one fixture clip.
type clipOpt struct {
	Camera     string // default "front"
	Start      time.Time
	Dur        time.Duration // default 30 s
	Incomplete bool
	Series     ulid.ULID // zero: the clip's own id
	Segment    int
	Triggers   []string // default ["motion"]
	NoVideo    bool
	NameID     ulid.ULID // filename ULID override (clip_id mismatch)
	Mutate     func(*sidecar.Sidecar)
}

func (f *fixture) newID(start time.Time) ulid.ULID {
	f.seq++
	var entropy [10]byte
	binary.BigEndian.PutUint64(entropy[2:], f.seq)
	id, err := ulid.New(ulid.Timestamp(start), bytes.NewReader(entropy[:]))
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// addClip writes the clip's sidecar and video and returns the sidecar as
// written (after Mutate).
func (f *fixture) addClip(o clipOpt) *sidecar.Sidecar {
	f.t.Helper()
	if o.Camera == "" {
		o.Camera = "front"
	}
	if o.Dur == 0 {
		o.Dur = 30 * time.Second
	}
	if o.Triggers == nil {
		o.Triggers = []string{"motion"}
	}
	id := f.newID(o.Start)
	series := o.Series
	if series == (ulid.ULID{}) {
		series = id
	}
	nameID := id
	if o.NameID != (ulid.ULID{}) {
		nameID = o.NameID
	}
	start := o.Start.UTC()
	keys := sidecar.ClipKeys(o.Camera, start, nameID)

	sc := &sidecar.Sidecar{
		Schema:       sidecar.Schema,
		Status:       sidecar.StatusComplete,
		ClipID:       id,
		SeriesID:     series,
		SegmentIndex: o.Segment,
		Camera:       sidecar.Camera{ID: o.Camera, Name: "Cam " + o.Camera, Brand: "tapo", Tags: []string{}},
		StartTime:    sidecar.Time{Time: start},
		PreRollMs:    0,
		EndReason:    "post_roll_elapsed",
		Video: sidecar.Video{
			Object:           sidecar.ObjectName(start, nameID, "zrec"),
			FormatVersion:    1,
			KeyID:            "a1b2c3d4e5f60718",
			ChunkSize:        1 << 20,
			SizeBytes:        1000,
			Container:        "fmp4",
			Codec:            "avc1.640032",
			Width:            1920,
			Height:           1080,
			InitSegmentBytes: 100,
		},
		Recorder: sidecar.Recorder{RelayID: "home-mini", Version: "2.0.0", TimeSource: "rtcp"},
	}
	for i, ty := range o.Triggers {
		sc.Triggers = append(sc.Triggers, sidecar.Trigger{Type: ty, FirstAtMs: int64(i), LastAtMs: int64(i) + 10})
	}
	if o.Incomplete {
		sc.Status = sidecar.StatusIncomplete
		sc.EndReason = "recovered"
	} else {
		end := sidecar.Time{Time: start.Add(o.Dur)}
		ms := o.Dur.Milliseconds()
		pt := int64(900)
		seek := [][2]int64{{0, 100}, {2000, 500}}
		sc.EndTime, sc.DurationMs = &end, &ms
		sc.Video.PlaintextSizeBytes = &pt
		sc.Video.SeekIndex = &seek
	}
	if o.Mutate != nil {
		o.Mutate(sc)
	}
	body, err := json.Marshal(sc)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.drv.PutSmall(ctx, keys.Sidecar, body, "application/json"); err != nil {
		f.t.Fatal(err)
	}
	if !o.NoVideo {
		f.writeVideo(keys.Video, id, start, o.Camera)
	}
	return sc
}

// writeVideo stores a small real zrec object at key.
func (f *fixture) writeVideo(key string, id ulid.ULID, start time.Time, cam string) {
	f.t.Helper()
	ctx := context.Background()
	w, err := f.drv.Create(ctx, key)
	if err != nil {
		f.t.Fatal(err)
	}
	zw, err := zrec.NewWriter(w, zrec.WriterParams{
		Keys:        zrec.Derive(zrec.MasterKey{1}),
		ClipID:      id,
		StartUnixMs: start.UnixMilli(),
		CameraID:    cam,
		ChunkSize:   zrecMinChunk,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := zw.Write([]byte("tiny plaintext")); err != nil {
		f.t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		f.t.Fatal(err)
	}
	if err := w.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

// zrecMinChunk is a small valid chunk size (a power of two in range).
const zrecMinChunk = 64 << 10

// remove deletes the object at key from the tree (simulates ILM expiry).
func (f *fixture) remove(key string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.base, testRoot, filepath.FromSlash(key))); err != nil {
		f.t.Fatal(err)
	}
}

// writeStatus publishes status/relay.json with the given camera ids.
func (f *fixture) writeStatus(updated time.Time, cams ...sidecar.CameraStatus) {
	f.t.Helper()
	st := sidecar.Status{
		Schema:    sidecar.StatusSchema,
		RelayID:   "home-mini",
		Version:   "2.0.0",
		UpdatedAt: sidecar.Time{Time: updated},
		KeyID:     "a1b2c3d4e5f60718",
		Cameras:   cams,
	}
	body, err := json.Marshal(st)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.drv.PutSmall(context.Background(), sidecar.StatusKey, body, "application/json"); err != nil {
		f.t.Fatal(err)
	}
}

func camStatus(id, rtsp string) sidecar.CameraStatus {
	return sidecar.CameraStatus{ID: id, Name: "Status " + id, RTSP: rtsp, Events: "up", Since: sidecar.Time{Time: testNow}}
}

// countingReader counts calls into a storage.ReadProvider.
type countingReader struct {
	storage.ReadProvider
	mu       sync.Mutex
	lists    []string // every List prefix, in order
	opened   []string // every OpenRange key, in order
	failList atomic.Bool
}

func (c *countingReader) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	c.mu.Lock()
	c.lists = append(c.lists, prefix)
	c.mu.Unlock()
	if c.failList.Load() {
		return func(yield func(storage.ObjectInfo, error) bool) {
			yield(storage.ObjectInfo{}, context.DeadlineExceeded)
		}
	}
	return c.ReadProvider.List(ctx, prefix)
}

func (c *countingReader) OpenRange(ctx context.Context, key string, off, n int64) (*storage.ObjectRange, error) {
	c.mu.Lock()
	c.opened = append(c.opened, key)
	c.mu.Unlock()
	return c.ReadProvider.OpenRange(ctx, key, off, n)
}

func (c *countingReader) listCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lists)
}

func (c *countingReader) listsOf(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.lists {
		if p == prefix {
			n++
		}
	}
	return n
}

func (c *countingReader) openCount(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, k := range c.opened {
		if k == key {
			n++
		}
	}
	return n
}

func (f *fixture) counting() *countingReader {
	return &countingReader{ReadProvider: f.drv}
}

func (f *fixture) index(r storage.ReadProvider, cfg IndexConfig) *Index {
	if cfg.HotRefreshSeconds == 0 {
		cfg.HotRefreshSeconds = 30
	}
	if cfg.ColdCacheDays == 0 {
		cfg.ColdCacheDays = 60
	}
	if cfg.AssumedMaxClipSeconds == 0 {
		cfg.AssumedMaxClipSeconds = 300
	}
	return NewIndex(r, cfg, f.clk)
}

func utc(y int, m time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, m, d, h, mi, s, 0, time.UTC)
}

// ids returns the clip ids of summaries, in order.
func ids(sums []Summary) []ulid.ULID {
	out := make([]ulid.ULID, len(sums))
	for i, s := range sums {
		out[i] = s.ClipID
	}
	return out
}

func sameIDs(got []Summary, want ...*sidecar.Sidecar) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].ClipID != want[i].ClipID {
			return false
		}
	}
	return true
}
