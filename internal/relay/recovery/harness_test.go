package recovery_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/relay/recovery"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// env is one storage backend under test.
type env struct {
	p storage.Provider
	r storage.Recoverer
	// pad is the number of bytes written after the zrec header: enough for a
	// part-based driver to have at least one completed part.
	pad int
	// olderThan is the candidate age the aged scenarios run with.
	olderThan time.Duration
	// crash abandons a writer without Commit or Abort (kill -9).
	crash func(t *testing.T, w storage.ObjectWriter)
	// age makes the given keys (and everything else written so far) old
	// enough for olderThan.
	age func(t *testing.T, keys ...string)
}

const camera = "front"

// testClip is one clip starting at a chosen time.
type testClip struct {
	start time.Time
	id    ulid.ULID
	keys  sidecar.ClipKeySet
}

func newClip(t *testing.T, cam string, start time.Time, n byte) testClip {
	t.Helper()
	start = start.UTC().Truncate(time.Millisecond)
	var entropy [10]byte
	entropy[9] = n
	id, err := ulid.New(uint64(start.UnixMilli()), bytes.NewReader(entropy[:]))
	if err != nil {
		t.Fatal(err)
	}
	return testClip{start: start, id: id, keys: sidecar.ClipKeys(cam, start, id)}
}

// header returns a real zrec header for the clip as written by zrec.NewWriter.
func (c testClip) header(t *testing.T, cam string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zrec.NewWriter(&buf, zrec.WriterParams{
		Keys:        zrec.Derive(zrec.MasterKey{1, 2, 3}),
		ClipID:      c.id,
		StartUnixMs: c.start.UnixMilli(),
		CameraID:    cam,
	})
	if err != nil {
		t.Fatalf("zrec.NewWriter: %v", err)
	}
	h := bytes.Clone(buf.Bytes()[:w.HeaderLen()])
	w.Abort()
	return h
}

// stagingDoc is the step-0 staging sidecar (Contract §7.3) for the clip,
// written under camera id cam.
func (c testClip) stagingDoc(t *testing.T, cam string) []byte {
	t.Helper()
	sc := sidecar.Sidecar{
		Schema:       sidecar.Schema,
		ClipID:       c.id,
		SeriesID:     c.id,
		SegmentIndex: 0,
		Camera:       sidecar.Camera{ID: cam, Name: "Front door", Brand: "tapo", Tags: []string{}},
		StartTime:    sidecar.Time{Time: c.start},
		PreRollMs:    4000,
		Video: sidecar.Video{
			Object:           c.keys.Video[len(c.keys.Dir)+1:],
			FormatVersion:    1,
			KeyID:            "a1b2c3d4e5f60718",
			ChunkSize:        1 << 20,
			Container:        "fmp4",
			Codec:            "avc1.640028",
			Width:            1920,
			Height:           1080,
			InitSegmentBytes: 812,
		},
		Recorder: sidecar.Recorder{RelayID: "relay-1", Version: "test", TimeSource: "arrival"},
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e env) putStaging(t *testing.T, c testClip, cam string) {
	t.Helper()
	if err := e.p.PutSmall(t.Context(), c.keys.StagingSidecar, c.stagingDoc(t, cam), "application/json"); err != nil {
		t.Fatalf("PutSmall(staging): %v", err)
	}
}

// crashVideo streams the clip's header (written under cam) and e.pad bytes,
// then abandons the writer. noHeader leaves an empty upload instead.
func (e env) crashVideo(t *testing.T, c testClip, cam string, noHeader bool) {
	t.Helper()
	w, err := e.p.Create(t.Context(), c.keys.Video)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !noHeader {
		data := append(c.header(t, cam), bytes.Repeat([]byte{0xa5}, e.pad)...)
		for off := 0; off < len(data); off += 1 << 20 {
			if _, err := w.Write(data[off:min(off+1<<20, len(data))]); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
	}
	e.crash(t, w)
}

// commitVideo stores a committed video for the clip (header + padding).
func (e env) commitVideo(t *testing.T, c testClip, cam string) {
	t.Helper()
	w, err := e.p.Create(t.Context(), c.keys.Video)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write(append(c.header(t, cam), bytes.Repeat([]byte{0x5a}, 4096)...)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func (e env) run(t *testing.T, cams ...string) (recovery.Report, error) {
	t.Helper()
	return recovery.Run(t.Context(), recovery.Config{
		Provider:  e.p,
		Recoverer: e.r,
		Cameras:   cams,
		Now:       time.Now().UTC(),
		OlderThan: e.olderThan,
	})
}

func (e env) mustRun(t *testing.T, cams ...string) recovery.Report {
	t.Helper()
	rep, err := e.run(t, cams...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func (e env) exists(t *testing.T, key string) bool {
	t.Helper()
	_, err := e.p.Stat(t.Context(), key)
	switch {
	case err == nil:
		return true
	case errors.Is(err, storage.ErrNotFound):
		return false
	}
	t.Fatalf("Stat(%q): %v", key, err)
	return false
}

func (e env) visible(t *testing.T, prefix string) []string {
	t.Helper()
	var keys []string
	for info, err := range e.p.List(t.Context(), prefix) {
		if err != nil {
			t.Fatalf("List(%q): %v", prefix, err)
		}
		keys = append(keys, info.Key)
	}
	slices.Sort(keys)
	return keys
}

func (e env) staged(t *testing.T) []string {
	t.Helper()
	infos, err := e.r.ListStaging(t.Context(), "")
	if err != nil {
		t.Fatalf("ListStaging: %v", err)
	}
	var keys []string
	for _, in := range infos {
		keys = append(keys, in.Key)
	}
	slices.Sort(keys)
	return keys
}

func (e env) readObject(t *testing.T, key string) []byte {
	t.Helper()
	or, err := e.p.OpenRange(t.Context(), key, 0, -1)
	if err != nil {
		t.Fatalf("OpenRange(%q): %v", key, err)
	}
	defer or.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(or); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e env) readSidecar(t *testing.T, key string) (*sidecar.Sidecar, []byte) {
	t.Helper()
	raw := e.readObject(t, key)
	sc, err := sidecar.Decode(raw)
	if err != nil {
		t.Fatalf("Decode(%q): %v", key, err)
	}
	return sc, raw
}

func wantReport(t *testing.T, got, want recovery.Report) {
	t.Helper()
	if got != want {
		t.Errorf("Report = %+v, want %+v", got, want)
	}
}

// hourAgo is the start of the test clips: always today or yesterday in UTC.
func hourAgo() time.Time { return time.Now().UTC().Add(-time.Hour) }

// runScenarios runs one sub-test per row of Contract §7.4's two tables, each
// on a fresh backend.
func runScenarios(t *testing.T, newEnv func(t *testing.T) env) {
	scenarios := []struct {
		name string
		fn   func(t *testing.T, e env)
	}{
		{"BothPresentRecoversIncompleteSidecar", testBothPresent},
		{"UploadWithoutStagingIsAborted", testUploadWithoutStaging},
		{"StagingAndCommittedVideoIsPromoted", testPromote},
		{"StagingWithRealSidecarIsDeleted", testStagingWithRealSidecar},
		{"StagingAloneIsDeleted", testStagingAlone},
		{"DescriptorMismatchLeavesVideoWithoutSidecar", testMismatch},
		{"PromoteMismatchLeavesVideoWithoutSidecar", testPromoteMismatch},
		{"EmptyUploadDiscardsThePair", testEmptyUpload},
		{"YoungerThanOlderThanIsUntouched", testYounger},
		{"OnlyTodayAndYesterdayOfListedCameras", testScope},
		{"SecondRunIsANoOp", testIdempotent},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t)
			t.Cleanup(func() { _ = e.p.Close() })
			s.fn(t, e)
		})
	}
}

func testBothPresent(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 1)
	e.putStaging(t, c, camera)
	e.crashVideo(t, c, camera, false)
	e.age(t, c.keys.Video, c.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Recovered: 1})

	st, err := e.p.Stat(t.Context(), c.keys.Video)
	if err != nil {
		t.Fatalf("recovered video missing: %v", err)
	}
	sc, raw := e.readSidecar(t, c.keys.Sidecar)
	if err := sc.Validate(); err != nil {
		t.Fatalf("recovered sidecar invalid: %v", err)
	}
	if sc.Status != sidecar.StatusIncomplete || sc.EndReason != "recovered" {
		t.Errorf("status/end_reason = %q/%q, want incomplete/recovered", sc.Status, sc.EndReason)
	}
	if sc.EndTime != nil || sc.DurationMs != nil || sc.Video.PlaintextSizeBytes != nil || sc.Video.SeekIndex != nil {
		t.Errorf("end_time/duration_ms/plaintext_size_bytes/seek_index must be null: %+v", sc)
	}
	for _, f := range []string{`"end_time":null`, `"duration_ms":null`, `"plaintext_size_bytes":null`, `"seek_index":null`, `"triggers":[]`} {
		if !bytes.Contains(raw, []byte(f)) {
			t.Errorf("sidecar JSON lacks %s: %s", f, raw)
		}
	}
	if sc.Video.SizeBytes != st.Size {
		t.Errorf("video.size_bytes = %d, want committed object size %d", sc.Video.SizeBytes, st.Size)
	}
	// Fields from the staging sidecar survive.
	if sc.Video.Codec != "avc1.640028" || sc.Video.InitSegmentBytes != 812 || sc.PreRollMs != 4000 || sc.ClipID != c.id {
		t.Errorf("staging fields lost: %+v", sc)
	}
	if got := e.staged(t); len(got) != 0 {
		t.Errorf("staging sidecars left: %q", got)
	}
	if got, want := e.visible(t, "clips"), []string{c.keys.Sidecar, c.keys.Video}; !slices.Equal(got, want) {
		t.Errorf("visible = %q, want %q", got, want)
	}
}

func testUploadWithoutStaging(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 2)
	e.crashVideo(t, c, camera, false)
	e.age(t, c.keys.Video)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Discarded: 1})

	if e.exists(t, c.keys.Video) || e.exists(t, c.keys.Sidecar) {
		t.Error("an upload without a staging sidecar left a visible object")
	}
	incs, err := e.r.ListIncomplete(t.Context(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 0 {
		t.Errorf("incomplete uploads remain: %+v", incs)
	}
}

func testPromote(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 3)
	e.putStaging(t, c, camera)
	e.commitVideo(t, c, camera)
	e.age(t, c.keys.Video, c.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Promoted: 1})

	st, err := e.p.Stat(t.Context(), c.keys.Video)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := e.readSidecar(t, c.keys.Sidecar)
	if err := sc.Validate(); err != nil {
		t.Fatalf("promoted sidecar invalid: %v", err)
	}
	if sc.Status != sidecar.StatusIncomplete || sc.EndReason != "recovered" || sc.Video.SizeBytes != st.Size {
		t.Errorf("promoted sidecar = %q/%q size %d, want incomplete/recovered/%d", sc.Status, sc.EndReason, sc.Video.SizeBytes, st.Size)
	}
	if got := e.staged(t); len(got) != 0 {
		t.Errorf("staging sidecars left: %q", got)
	}
}

func testStagingWithRealSidecar(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 4)
	e.putStaging(t, c, camera)
	e.commitVideo(t, c, camera)
	real := []byte(`{"schema":"zrec.sidecar/2","marker":"the-real-one"}`)
	if err := e.p.PutSmall(t.Context(), c.keys.Sidecar, real, "application/json"); err != nil {
		t.Fatal(err)
	}
	e.age(t, c.keys.Video, c.keys.Sidecar, c.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Discarded: 1})

	if got := e.staged(t); len(got) != 0 {
		t.Errorf("staging sidecar not deleted: %q", got)
	}
	if got := e.readObject(t, c.keys.Sidecar); !bytes.Equal(got, real) {
		t.Errorf("real sidecar was overwritten: %s", got)
	}
}

func testStagingAlone(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 5)
	e.putStaging(t, c, camera)
	e.age(t, c.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Discarded: 1})

	if got := e.staged(t); len(got) != 0 {
		t.Errorf("staging sidecar not deleted: %q", got)
	}
	if got := e.visible(t, "clips"); len(got) != 0 {
		t.Errorf("visible = %q, want none", got)
	}
}

func testMismatch(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 6)
	e.putStaging(t, c, "other-cam") // the staging sidecar lies about camera_id
	e.crashVideo(t, c, camera, false)
	e.age(t, c.keys.Video, c.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Orphaned: 1})

	if !e.exists(t, c.keys.Video) {
		t.Error("the committed video was removed; it must be left in place")
	}
	if e.exists(t, c.keys.Sidecar) {
		t.Error("a sidecar was published for a mismatched pair")
	}
	if got := e.staged(t); len(got) != 0 {
		t.Errorf("staging sidecar not deleted: %q", got)
	}
	// A video is visible iff its sidecar exists (Contract §7.3): the listing
	// holds the video and nothing else, so no reader will show it.
	if got, want := e.visible(t, "clips"), []string{c.keys.Video}; !slices.Equal(got, want) {
		t.Errorf("listing = %q, want only the sidecar-less video %q", got, want)
	}
}

func testPromoteMismatch(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 14)
	e.putStaging(t, c, "other-cam")
	e.commitVideo(t, c, camera)
	e.age(t, c.keys.Video, c.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Orphaned: 1})

	if !e.exists(t, c.keys.Video) || e.exists(t, c.keys.Sidecar) {
		t.Error("want the committed video left in place without a sidecar")
	}
	if got := e.staged(t); len(got) != 0 {
		t.Errorf("staging sidecar not deleted: %q", got)
	}
}

func testEmptyUpload(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 7)
	e.putStaging(t, c, camera)
	e.crashVideo(t, c, camera, true) // crash before the first write(2) reached the file
	e.age(t, c.keys.Video, c.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Discarded: 1})

	if e.exists(t, c.keys.Video) || e.exists(t, c.keys.Sidecar) {
		t.Error("an unreadable-descriptor pair left a visible object")
	}
	if got := e.staged(t); len(got) != 0 {
		t.Errorf("staging sidecar not deleted: %q", got)
	}
}

func testYounger(t *testing.T, e env) {
	both := newClip(t, camera, hourAgo(), 8)
	alone := newClip(t, camera, hourAgo().Add(time.Second), 9)
	e.putStaging(t, both, camera)
	e.crashVideo(t, both, camera, false)
	e.putStaging(t, alone, camera)
	// No age(): everything was just written, so it is younger than OlderThan.
	rep, err := recovery.Run(t.Context(), recovery.Config{
		Provider: e.p, Recoverer: e.r, Cameras: []string{camera},
		Now: time.Now().UTC(), OlderThan: time.Hour,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReport(t, rep, recovery.Report{})
	if got := e.staged(t); len(got) != 2 {
		t.Errorf("staging sidecars = %q, want both untouched", got)
	}
	if e.exists(t, both.keys.Sidecar) {
		t.Error("a young clip was recovered")
	}
	incs, err := e.r.ListIncomplete(t.Context(), "", 0)
	if err != nil || len(incs) != 1 {
		t.Errorf("young crashed video not left in place: %+v, %v", incs, err)
	}
}

func testScope(t *testing.T, e env) {
	now := time.Now().UTC()
	old := newClip(t, camera, now.AddDate(0, 0, -2), 10)
	notListed := newClip(t, "garage", hourAgo(), 11)
	yesterday := newClip(t, camera, now.AddDate(0, 0, -1), 12)
	e.putStaging(t, old, camera)
	e.putStaging(t, notListed, "garage")
	e.putStaging(t, yesterday, camera)
	e.age(t, old.keys.StagingSidecar, notListed.keys.StagingSidecar, yesterday.keys.StagingSidecar)

	wantReport(t, e.mustRun(t, camera), recovery.Report{Discarded: 1})

	want := []string{notListed.keys.StagingSidecar, old.keys.StagingSidecar}
	slices.Sort(want)
	if got := e.staged(t); !slices.Equal(got, want) {
		t.Errorf("remaining staging = %q, want %q (only yesterday's of the listed camera is reaped)", got, want)
	}
}

func testIdempotent(t *testing.T, e env) {
	c := newClip(t, camera, hourAgo(), 13)
	e.putStaging(t, c, camera)
	e.crashVideo(t, c, camera, false)
	e.age(t, c.keys.Video, c.keys.StagingSidecar)
	wantReport(t, e.mustRun(t, camera), recovery.Report{Recovered: 1})
	wantReport(t, e.mustRun(t, camera), recovery.Report{})
}
