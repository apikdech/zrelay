package recorder_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/clip"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/mux"
	"github.com/apikdech/zrelay-v2/internal/relay/recorder"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
	"github.com/apikdech/zrelay-v2/internal/testutil"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/local"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const (
	h264Fixture = "../../../testdata/video/h264-640x360-gop2-10s.h264"
	fps         = 15
	tick        = 90000 / fps
	cameraID    = "front-door"
)

var t0 = time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC)

func testKeys() zrec.Keys {
	var m zrec.MasterKey
	for i := range m {
		m[i] = byte(i + 1)
	}
	return zrec.Derive(m)
}

func keyIDHex() string {
	id := testKeys().ID
	return hex.EncodeToString(id[:])
}

func testCamera() config.Camera {
	return config.Camera{ID: cameraID, Name: "Front Door", Brand: "tapo", Tags: []string{"exterior", "entrance"}}
}

// fixture loads the H.264 fixture and returns its codec and GOPs, timed like a
// camera session would (DTS from 0, Wall from start). Parameter sets move into
// the codec, as the RTSP session does.
func fixture(t testing.TB, start time.Time) (camera.Codec, []*ring.GOP) {
	t.Helper()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	c := camera.Codec{Kind: "h264", Width: 640, Height: 360}
	var gops []*ring.GOP
	for i, au := range aus {
		var nalus [][]byte
		var size int
		for _, n := range au.NALUs {
			switch n[0] & 0x1f {
			case 7:
				c.SPS = n
			case 8:
				c.PPS = n
			default:
				nalus = append(nalus, n)
				size += len(n)
			}
		}
		a := &camera.AccessUnit{
			PTS: int64(i) * tick, DTS: int64(i) * tick, IDR: au.IDR, NALUs: nalus,
			Wall: start.Add(time.Duration(i) * time.Second / fps), Bytes: size,
		}
		if au.IDR {
			gops = append(gops, &ring.GOP{Start: a.Wall, StartDTS: a.DTS})
		}
		g := gops[len(gops)-1]
		g.AUs = append(g.AUs, a)
		g.Bytes += int64(size)
	}
	return c, gops
}

func openInfo(t testing.TB, c camera.Codec, start time.Time, segment int) clip.OpenInfo {
	t.Helper()
	id, err := ulid.New(ulid.Timestamp(start), bytes.NewReader(bytes.Repeat([]byte{7}, 10)))
	if err != nil {
		t.Fatal(err)
	}
	series := id
	pre := int64(1800)
	if segment > 0 {
		pre = 0
	}
	return clip.OpenInfo{
		ClipID: id, SeriesID: series, SegmentIndex: segment, Start: start,
		PreRollMs: pre, Codec: c, TimeSource: camera.TimeSourceRTCP,
	}
}

// spy wraps a storage.Writer and records every call in order and every byte
// written to storage.
type spy struct {
	storage.Writer

	mu    sync.Mutex
	ops   []string
	small map[string][]byte
	video bytes.Buffer
}

func newSpy(w storage.Writer) *spy { return &spy{Writer: w, small: map[string][]byte{}} }

func (s *spy) record(op string) {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	s.mu.Unlock()
}

func (s *spy) opLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ops)
}

func (s *spy) PutSmall(ctx context.Context, key string, data []byte, ct string) error {
	s.record("putsmall " + key)
	s.mu.Lock()
	s.small[key] = bytes.Clone(data)
	s.mu.Unlock()
	return s.Writer.PutSmall(ctx, key, data, ct)
}

func (s *spy) Delete(ctx context.Context, key string) error {
	s.record("delete " + key)
	return s.Writer.Delete(ctx, key)
}

func (s *spy) Create(ctx context.Context, key string) (storage.ObjectWriter, error) {
	s.record("create " + key)
	ow, err := s.Writer.Create(ctx, key)
	if err != nil {
		return nil, err
	}
	return &spyObject{ObjectWriter: ow, s: s, key: key}, nil
}

type spyObject struct {
	storage.ObjectWriter
	s   *spy
	key string
}

func (o *spyObject) Write(p []byte) (int, error) {
	o.s.record("write " + o.key)
	o.s.mu.Lock()
	o.s.video.Write(p)
	o.s.mu.Unlock()
	return o.ObjectWriter.Write(p)
}

func (o *spyObject) Commit(ctx context.Context) error {
	o.s.record("commit " + o.key)
	return o.ObjectWriter.Commit(ctx)
}

func (o *spyObject) Abort(ctx context.Context) error {
	o.s.record("abort " + o.key)
	return o.ObjectWriter.Abort(ctx)
}

func newLocal(t testing.TB) (*local.Driver, string) {
	t.Helper()
	base := t.TempDir()
	d, err := local.New(base, "")
	if err != nil {
		t.Fatal(err)
	}
	return d, base
}

func newRecorder(t testing.TB, w storage.Writer, chunk int) *recorder.Recorder {
	t.Helper()
	return recorder.New(recorder.Config{
		Camera: testCamera(), RelayID: "home-mini", Version: "2.0.0-test",
		Keys: testKeys(), ChunkSize: chunk, Clock: clock.Real,
	}, recorder.DirectSink{W: w})
}

func readFile(t testing.TB, base, key string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(base, key string) bool {
	_, err := os.Stat(filepath.Join(base, filepath.FromSlash(key)))
	return err == nil
}

func decrypt(t testing.TB, video []byte) []byte {
	t.Helper()
	k := testKeys()
	zr, err := zrec.OpenReader(bytes.NewReader(video), int64(len(video)), zrec.Keyring{k.ID: k}, zrec.ReaderOptions{})
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	var pt bytes.Buffer
	if _, err := zr.WriteTo(&pt); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return pt.Bytes()
}

// recordClip opens a clip, writes the first n fixture GOPs and finalizes it.
func recordClip(t testing.TB, rec *recorder.Recorder, c camera.Codec, gops []*ring.GOP, info clip.OpenInfo, fin clip.FinalInfo) {
	t.Helper()
	w, err := rec.Open(info)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, g := range gops {
		if err := w.WriteGOP(g); err != nil {
			t.Fatalf("WriteGOP: %v", err)
		}
	}
	if err := w.Finalize(fin); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

func TestFinalSidecarDescribesClip(t *testing.T) {
	d, base := newLocal(t)
	rec := newRecorder(t, d, 64<<10)
	c, gops := fixture(t, t0)
	info := openInfo(t, c, t0, 0)
	fin := clip.FinalInfo{
		End: t0.Add(4 * time.Second), DurationMs: 4000, Reason: clip.ReasonPostRollElapsed,
		Triggers: []clip.TriggerRecord{{Kind: events.KindMotion, FirstAtMs: 1800, LastAtMs: 3000, RawTopics: []string{"tns1:x"}}},
	}
	recordClip(t, rec, c, gops[:2], info, fin)

	keys := sidecar.ClipKeys(cameraID, t0, info.ClipID)
	sc, err := sidecar.Decode(readFile(t, base, keys.Sidecar))
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sc.Status != sidecar.StatusComplete || sc.EndReason != "post_roll_elapsed" || sc.ClipID != info.ClipID ||
		sc.SeriesID != info.SeriesID || sc.PreRollMs != 1800 || sc.StartTime.UnixMilli() != t0.UnixMilli() {
		t.Errorf("sidecar header fields wrong: %+v", sc)
	}
	if sc.Camera.ID != cameraID || sc.Camera.Name != "Front Door" || sc.Camera.Brand != "tapo" || !slices.Equal(sc.Camera.Tags, []string{"exterior", "entrance"}) {
		t.Errorf("camera = %+v", sc.Camera)
	}
	if sc.DurationMs == nil || *sc.DurationMs != 4000 || sc.EndTime == nil || sc.EndTime.UnixMilli() != t0.Add(4*time.Second).UnixMilli() {
		t.Errorf("end/duration = %v / %v", sc.EndTime, sc.DurationMs)
	}
	if len(sc.Triggers) != 1 || sc.Triggers[0].Type != "motion" || sc.Triggers[0].FirstAtMs != 1800 ||
		sc.Triggers[0].LastAtMs != 3000 || !slices.Equal(sc.Triggers[0].RawTopics, []string{"tns1:x"}) {
		t.Errorf("triggers = %+v", sc.Triggers)
	}
	if sc.Recorder != (sidecar.Recorder{RelayID: "home-mini", Version: "2.0.0-test", TimeSource: "rtcp"}) {
		t.Errorf("recorder = %+v", sc.Recorder)
	}
	video := readFile(t, base, keys.Video)
	if sc.Video.SizeBytes != int64(len(video)) {
		t.Errorf("size_bytes = %d, file is %d", sc.Video.SizeBytes, len(video))
	}
	if sc.Video.ChunkSize != 64<<10 || sc.Video.KeyID != keyIDHex() {
		t.Errorf("chunk_size/key_id = %d / %s", sc.Video.ChunkSize, sc.Video.KeyID)
	}
	pt := decrypt(t, video)
	if sc.Video.PlaintextSizeBytes == nil || *sc.Video.PlaintextSizeBytes != int64(len(pt)) {
		t.Errorf("plaintext_size_bytes = %v, decrypted %d", sc.Video.PlaintextSizeBytes, len(pt))
	}
	if exists(base, keys.StagingSidecar) {
		t.Error("staging sidecar was not deleted after the final sidecar")
	}
}

func TestContinuationSegmentSidecar(t *testing.T) {
	d, base := newLocal(t)
	rec := newRecorder(t, d, 0)
	c, gops := fixture(t, t0)
	info := openInfo(t, c, t0, 1)
	recordClip(t, rec, c, gops[:1], info, clip.FinalInfo{End: t0.Add(2 * time.Second), DurationMs: 2000, Reason: clip.ReasonMaxDuration})
	sc, err := sidecar.Decode(readFile(t, base, sidecar.ClipKeys(cameraID, t0, info.ClipID).Sidecar))
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	if sc.SegmentIndex != 1 || sc.PreRollMs != 0 || sc.EndReason != "max_duration" || sc.Triggers == nil || len(sc.Triggers) != 0 {
		t.Errorf("segment fields wrong: index %d pre-roll %d reason %s triggers %v", sc.SegmentIndex, sc.PreRollMs, sc.EndReason, sc.Triggers)
	}
	if sc.Video.ChunkSize != 1<<20 {
		t.Errorf("default chunk_size = %d, want 1 MiB", sc.Video.ChunkSize)
	}
}

func TestKeysUseSegmentStartDay(t *testing.T) {
	d, base := newLocal(t)
	rec := newRecorder(t, d, 0)
	start := time.Date(2026, 11, 7, 0, 0, 0, 500_000_000, time.UTC)
	c, gops := fixture(t, start)
	info := openInfo(t, c, start, 1)
	recordClip(t, rec, c, gops[:1], info, clip.FinalInfo{End: start.Add(2 * time.Second), DurationMs: 2000, Reason: clip.ReasonPostRollElapsed})
	if !exists(base, "clips/"+cameraID+"/2026/11/07/000000500_"+info.ClipID.String()+".json") {
		t.Error("sidecar not under the segment's own UTC day")
	}
}

func TestStagingSidecarWrittenBeforeFirstChunk(t *testing.T) {
	d, base := newLocal(t)
	sp := newSpy(d)
	rec := newRecorder(t, sp, 64<<10)
	c, gops := fixture(t, t0)
	info := openInfo(t, c, t0, 0)
	keys := sidecar.ClipKeys(cameraID, t0, info.ClipID)
	recordClip(t, rec, c, gops, info, clip.FinalInfo{End: t0.Add(6 * time.Second), DurationMs: 6000, Reason: clip.ReasonPostRollElapsed})

	ops := sp.opLog()
	stage := slices.Index(ops, "putsmall "+keys.StagingSidecar)
	create := slices.Index(ops, "create "+keys.Video)
	firstWrite := slices.Index(ops, "write "+keys.Video)
	if stage < 0 || create < 0 || firstWrite < 0 {
		t.Fatalf("missing operations in %v", ops)
	}
	if !(stage < firstWrite) || !(stage < create) {
		t.Errorf("staging sidecar must precede the first write to the video: %v", ops)
	}
	commit := slices.Index(ops, "commit "+keys.Video)
	final := slices.Index(ops, "putsmall "+keys.Sidecar)
	del := slices.Index(ops, "delete "+keys.StagingSidecar)
	if !(firstWrite < commit && commit < final && final < del) {
		t.Errorf("want write < commit < final sidecar < delete staging, got %v", ops)
	}
	// Streaming, not buffering: more than one write reached the object.
	writes := 0
	for _, op := range ops {
		if op == "write "+keys.Video {
			writes++
		}
	}
	if writes < 2 {
		t.Errorf("only %d writes to the object; the writer should stream chunks", writes)
	}
	_ = base
}

func TestStagingSidecarHasAllRecoveryFields(t *testing.T) {
	d, _ := newLocal(t)
	sp := newSpy(d)
	rec := newRecorder(t, sp, 0)
	c, gops := fixture(t, t0)
	info := openInfo(t, c, t0, 0)
	keys := sidecar.ClipKeys(cameraID, t0, info.ClipID)

	w, err := rec.Open(info)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort(clip.ReasonShutdown)
	raw := sp.small[keys.StagingSidecar]
	if raw == nil {
		t.Fatal("no staging sidecar written by Open")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"schema", "clip_id", "series_id", "segment_index", "camera", "start_time", "pre_roll_ms", "video", "recorder"} {
		if _, ok := doc[f]; !ok {
			t.Errorf("staging sidecar lacks %q", f)
		}
	}
	vid, _ := doc["video"].(map[string]any)
	for _, f := range []string{"object", "format_version", "key_id", "chunk_size", "container", "codec", "width", "height", "init_segment_bytes"} {
		if _, ok := vid[f]; !ok {
			t.Errorf("staging sidecar lacks video.%s", f)
		}
	}

	sc, err := sidecar.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantCodec, err := mux.CodecString(c)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ClipID != info.ClipID || sc.SeriesID != info.SeriesID || sc.SegmentIndex != 0 || sc.PreRollMs != 1800 ||
		sc.Camera.ID != cameraID || sc.StartTime.UnixMilli() != t0.UnixMilli() {
		t.Errorf("identity fields wrong: %+v", sc)
	}
	v := sc.Video
	if v.Object != sidecar.ObjectName(t0, info.ClipID, "zrec") || v.FormatVersion != 1 || v.KeyID != keyIDHex() ||
		v.ChunkSize != 1<<20 || v.Container != "fmp4" || v.Codec != wantCodec || v.Width != 640 || v.Height != 360 {
		t.Errorf("video = %+v", v)
	}
	var probe bytes.Buffer
	if _, err := mux.New(&probe, c); err != nil {
		t.Fatal(err)
	}
	if v.InitSegmentBytes != int64(probe.Len()) {
		t.Errorf("init_segment_bytes = %d, init segment is %d bytes", v.InitSegmentBytes, probe.Len())
	}
	// What recovery does (Contract §7.4) must yield a valid sidecar.
	sc.Status, sc.EndReason, sc.EndTime, sc.DurationMs = sidecar.StatusIncomplete, "recovered", nil, nil
	sc.Triggers, sc.Video.SizeBytes, sc.Video.PlaintextSizeBytes, sc.Video.SeekIndex = []sidecar.Trigger{}, 1234, nil, nil
	if err := sc.Validate(); err != nil {
		t.Errorf("recovered form of the staging sidecar does not validate: %v", err)
	}
	_ = gops
}

func TestNoPlaintextReachesSink(t *testing.T) {
	d, base := newLocal(t)
	sp := newSpy(d)
	rec := newRecorder(t, sp, 64<<10)
	c, gops := fixture(t, t0)
	info := openInfo(t, c, t0, 0)
	recordClip(t, rec, c, gops, info, clip.FinalInfo{End: t0.Add(6 * time.Second), DurationMs: 6000, Reason: clip.ReasonPostRollElapsed})

	if len(c.SPS) < 8 {
		t.Fatalf("fixture SPS too short: %d", len(c.SPS))
	}
	sp.mu.Lock()
	sunk := bytes.Clone(sp.video.Bytes())
	for _, b := range sp.small {
		sunk = append(sunk, b...)
	}
	sp.mu.Unlock()
	if len(sunk) == 0 {
		t.Fatal("nothing reached the sink")
	}
	for name, nalu := range map[string][]byte{"SPS": c.SPS, "PPS": c.PPS, "ftyp": []byte("ftypiso"), "moof": []byte("moof"), "mdat": []byte("mdat")} {
		if bytes.Contains(sunk, nalu) {
			t.Errorf("sink bytes contain plaintext %s", name)
		}
	}
	// Control: the decrypted clip does carry the SPS, so the check above can fail.
	video := readFile(t, base, sidecar.ClipKeys(cameraID, t0, info.ClipID).Video)
	if !bytes.Contains(decrypt(t, video), c.SPS) {
		t.Error("control: decrypted clip lacks the SPS")
	}
}

func TestAbortDiscardsObjectAndStaging(t *testing.T) {
	d, base := newLocal(t)
	sp := newSpy(d)
	rec := newRecorder(t, sp, 0)
	c, _ := fixture(t, t0)
	info := openInfo(t, c, t0, 0)
	keys := sidecar.ClipKeys(cameraID, t0, info.ClipID)
	w, err := rec.Open(info)
	if err != nil {
		t.Fatal(err)
	}
	w.Abort(clip.ReasonStreamLost)
	if exists(base, keys.Video) || exists(base, keys.Sidecar) || exists(base, keys.StagingSidecar) {
		t.Error("an aborted clip left objects behind")
	}
	if ops := sp.opLog(); !slices.Contains(ops, "abort "+keys.Video) || !slices.Contains(ops, "delete "+keys.StagingSidecar) {
		t.Errorf("ops = %v", ops)
	}
	var left []string
	_ = filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
}

// failWriter fails the named operation.
type failWriter struct {
	storage.Writer
	failPut, failCreate bool
}

func (f failWriter) PutSmall(ctx context.Context, key string, data []byte, ct string) error {
	if f.failPut {
		return errors.New("put failed")
	}
	return f.Writer.PutSmall(ctx, key, data, ct)
}

func (f failWriter) Create(ctx context.Context, key string) (storage.ObjectWriter, error) {
	if f.failCreate {
		return nil, errors.New("create failed")
	}
	return f.Writer.Create(ctx, key)
}

func TestOpenFailsWhenStorageRefuses(t *testing.T) {
	c, _ := fixture(t, t0)
	info := openInfo(t, c, t0, 0)
	keys := sidecar.ClipKeys(cameraID, t0, info.ClipID)

	d, base := newLocal(t)
	if _, err := newRecorder(t, failWriter{Writer: d, failPut: true}, 0).Open(info); err == nil {
		t.Error("Open succeeded although the staging sidecar could not be written")
	}
	d2, base2 := newLocal(t)
	if _, err := newRecorder(t, failWriter{Writer: d2, failCreate: true}, 0).Open(info); err == nil {
		t.Error("Open succeeded although the object could not be created")
	}
	if exists(base2, keys.StagingSidecar) {
		t.Error("staging sidecar left after a failed Create")
	}
	if exists(base, keys.StagingSidecar) {
		t.Error("staging sidecar present after a failed put")
	}
}

// fakeOutput records the clips the machine opens, for the Run test.
type fakeOutput struct {
	mu      sync.Mutex
	writers []*fakeWriter
}

type fakeWriter struct {
	mu      sync.Mutex
	info    clip.OpenInfo
	gops    int
	final   *clip.FinalInfo
	aborted bool
}

func (o *fakeOutput) Open(i clip.OpenInfo) (clip.ClipWriter, error) {
	w := &fakeWriter{info: i}
	o.mu.Lock()
	o.writers = append(o.writers, w)
	o.mu.Unlock()
	return w, nil
}

func (o *fakeOutput) last() *fakeWriter {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.writers) == 0 {
		return nil
	}
	return o.writers[len(o.writers)-1]
}

func (w *fakeWriter) WriteGOP(*ring.GOP) error {
	w.mu.Lock()
	w.gops++
	w.mu.Unlock()
	return nil
}
func (w *fakeWriter) Finalize(f clip.FinalInfo) error {
	w.mu.Lock()
	w.final = &f
	w.mu.Unlock()
	return nil
}
func (w *fakeWriter) Abort(clip.EndReason) {
	w.mu.Lock()
	w.aborted = true
	w.mu.Unlock()
}
func (w *fakeWriter) finalInfo() *clip.FinalInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.final
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunTranslatesHubAndTrackerIntoMachineInputs(t *testing.T) {
	fc := clock.NewFake(t0)
	hub := camera.NewHub(cameraID)
	sub := hub.Subscribe(camera.ClassRecorder, 4096)
	tracker := events.NewTracker(time.Minute, fc)
	out := &fakeOutput{}
	rg := ring.New(time.Second, 64<<20, nil)
	m := clip.New(clip.Config{Camera: cameraID, PostRoll: 5 * time.Second}, fc, rg, out)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { recorder.Run(ctx, sub, tracker, m); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	c, gops := fixture(t, t0)
	hub.Codec(c, camera.TimeSourceRTCP)
	feed := func(g *ring.GOP) {
		for _, au := range g.AUs {
			hub.Video(au)
		}
	}
	feed(gops[0])
	tracker.Apply(events.Signal{Camera: cameraID, Kind: events.KindPerson, Active: true, At: time.Now()})
	feed(gops[1]) // the IDR closes GOP 0 into the clip
	eventually(t, "state Recording", func() bool { return m.State() == "Recording" })
	w := out.last()
	if w == nil || w.info.Codec.Kind != "h264" || w.info.TimeSource != camera.TimeSourceRTCP {
		t.Fatalf("clip not opened with the hub codec: %+v", w)
	}

	tracker.Apply(events.Signal{Camera: cameraID, Kind: events.KindPerson, Active: false, At: time.Now()})
	eventually(t, "state PostRoll", func() bool { return m.State() == "PostRoll" })

	// The 250 ms tick ends the clip once the post-roll has elapsed.
	fc.Advance(6 * time.Second)
	eventually(t, "clip finalized by Tick", func() bool { return w.finalInfo() != nil })
	f := w.finalInfo()
	if f.Reason != clip.ReasonPostRollElapsed {
		t.Errorf("reason = %s", f.Reason)
	}
	if len(f.Triggers) != 1 || f.Triggers[0].Kind != events.KindPerson {
		t.Errorf("triggers = %+v, want one person trigger", f.Triggers)
	}
	if m.State() != "Idle" {
		t.Errorf("state = %s, want Idle", m.State())
	}
}

func TestRunShutdownFinalizesOpenClip(t *testing.T) {
	fc := clock.NewFake(t0)
	hub := camera.NewHub(cameraID)
	sub := hub.Subscribe(camera.ClassRecorder, 4096)
	tracker := events.NewTracker(time.Minute, fc)
	out := &fakeOutput{}
	m := clip.New(clip.Config{Camera: cameraID, PostRoll: 5 * time.Second}, fc, ring.New(time.Second, 64<<20, nil), out)

	// The tracker is already triggered when Run starts.
	tracker.Apply(events.Signal{Camera: cameraID, Kind: events.KindMotion, Active: true, At: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { recorder.Run(ctx, sub, tracker, m); close(done) }()

	c, gops := fixture(t, t0)
	hub.Codec(c, camera.TimeSourceArrival)
	for _, g := range gops[:2] {
		for _, au := range g.AUs {
			hub.Video(au)
		}
	}
	eventually(t, "state Recording", func() bool { return m.State() == "Recording" })
	w := out.last()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	f := w.finalInfo()
	if f == nil || f.Reason != clip.ReasonShutdown {
		t.Fatalf("final = %+v, want shutdown", f)
	}
	if m.State() != "Closed" {
		t.Errorf("state = %s, want Closed", m.State())
	}
}

func TestRunLostEndsClipWithStreamLost(t *testing.T) {
	fc := clock.NewFake(t0)
	hub := camera.NewHub(cameraID)
	sub := hub.Subscribe(camera.ClassRecorder, 4096)
	tracker := events.NewTracker(time.Minute, fc)
	out := &fakeOutput{}
	m := clip.New(clip.Config{Camera: cameraID, PostRoll: 5 * time.Second}, fc, ring.New(time.Second, 64<<20, nil), out)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { recorder.Run(ctx, sub, tracker, m); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	tracker.Apply(events.Signal{Camera: cameraID, Kind: events.KindMotion, Active: true, At: time.Now()})
	c, gops := fixture(t, t0)
	hub.Codec(c, camera.TimeSourceArrival)
	for _, g := range gops[:2] {
		for _, au := range g.AUs {
			hub.Video(au)
		}
	}
	eventually(t, "state Recording", func() bool { return m.State() == "Recording" })
	w := out.last()
	hub.Lost()
	eventually(t, "clip finalized", func() bool { return w.finalInfo() != nil })
	if r := w.finalInfo().Reason; r != clip.ReasonStreamLost {
		t.Errorf("reason = %s, want stream_lost", r)
	}
}

// TestRecorderEndToEndLocal runs the whole chain: RTSP test server, Hub,
// Machine, Recorder and DirectSink on the local driver.
func TestRecorderEndToEndLocal(t *testing.T) {
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: testutil.LoadAnnexB(t, h264Fixture), Loop: true, FPS: fps})
	d, base := newLocal(t)
	rec := newRecorder(t, d, 64<<10)

	hub := camera.NewHub(cameraID)
	sub := hub.Subscribe(camera.ClassRecorder, 512)
	tracker := events.NewTracker(time.Minute, clock.Real)
	m := clip.New(clip.Config{Camera: cameraID, PostRoll: 500 * time.Millisecond}, clock.Real, ring.New(time.Second, 64<<20, nil), rec)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = camera.RunRTSP(ctx, srv.URL(), hub) }()
	go func() { defer wg.Done(); recorder.Run(ctx, sub, tracker, m) }()
	t.Cleanup(func() { cancel(); wg.Wait() })

	eventually(t, "camera codec", func() bool { _, _, ok := hub.CurrentCodec(); return ok })
	codec, _, _ := hub.CurrentCodec()

	// Scripted trigger: active for 4 s.
	tracker.Apply(events.Signal{Camera: cameraID, Kind: events.KindMotion, Active: true, At: time.Now()})
	eventually(t, "state Recording", func() bool { return m.State() == "Recording" })
	if err := clock.Real.Sleep(ctx, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	tracker.Apply(events.Signal{Camera: cameraID, Kind: events.KindMotion, Active: false, At: time.Now()})

	var sidecarKey string
	eventually(t, "sidecar after post-roll", func() bool {
		_ = filepath.WalkDir(filepath.Join(base, "clips"), func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(p, ".json") && !strings.HasPrefix(e.Name(), ".") {
				rel, _ := filepath.Rel(base, p)
				sidecarKey = filepath.ToSlash(rel)
			}
			return nil
		})
		return sidecarKey != ""
	})
	cancel()
	wg.Wait()

	sc, err := sidecar.Decode(readFile(t, base, sidecarKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("sidecar invalid: %v", err)
	}
	wantCodec, err := mux.CodecString(codec)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Video.Codec != wantCodec {
		t.Errorf("video.codec = %q, want %q", sc.Video.Codec, wantCodec)
	}
	if sc.Status != sidecar.StatusComplete || sc.EndReason != "post_roll_elapsed" {
		t.Errorf("status %s / end_reason %s", sc.Status, sc.EndReason)
	}
	if len(sc.Triggers) != 1 || sc.Triggers[0].Type != "motion" {
		t.Errorf("triggers = %+v", sc.Triggers)
	}
	if sc.DurationMs == nil || *sc.DurationMs < 3000 {
		t.Errorf("duration_ms = %v, want at least the 4 s trigger less one GOP", sc.DurationMs)
	}

	keys := sidecar.ClipKeys(cameraID, sc.StartTime.Time, sc.ClipID)
	if keys.Sidecar != sidecarKey {
		t.Fatalf("sidecar at %q, keys derive %q", sidecarKey, keys.Sidecar)
	}
	video := readFile(t, base, keys.Video)
	if sc.Video.SizeBytes != int64(len(video)) {
		t.Errorf("size_bytes %d, file %d", sc.Video.SizeBytes, len(video))
	}
	pt := decrypt(t, video)
	if sc.Video.PlaintextSizeBytes == nil || *sc.Video.PlaintextSizeBytes != int64(len(pt)) {
		t.Fatalf("plaintext_size_bytes = %v, decrypted %d", sc.Video.PlaintextSizeBytes, len(pt))
	}
	if sc.Video.SeekIndex == nil || len(*sc.Video.SeekIndex) == 0 {
		t.Fatal("no seek index")
	}
	for i, e := range *sc.Video.SeekIndex {
		if e[1] < sc.Video.InitSegmentBytes || e[1] >= *sc.Video.PlaintextSizeBytes {
			t.Errorf("seek_index[%d] offset %d outside [%d, %d)", i, e[1], sc.Video.InitSegmentBytes, *sc.Video.PlaintextSizeBytes)
		}
		if !bytes.Equal(pt[e[1]+4:e[1]+8], []byte("moof")) {
			t.Errorf("seek_index[%d] offset %d does not point at a moof", i, e[1])
		}
	}
	if exists(base, keys.StagingSidecar) {
		t.Error("staging sidecar left behind")
	}

	out := testutil.FFProbe(t, pt, "-show_streams", "-of", "json")
	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("ffprobe output: %v\n%s", err, out)
	}
	if len(probe.Streams) != 1 || probe.Streams[0].CodecName != "h264" || probe.Streams[0].Width != 640 || probe.Streams[0].Height != 360 {
		t.Errorf("ffprobe streams = %+v", probe.Streams)
	}
}
