package clip_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/clip"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
)

var t0 = time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC)

const (
	fps      = 15
	step     = time.Second / fps
	gopLen   = 2 * fps // frames per GOP: 2 s at 15 fps
	preRoll  = 15 * time.Second
	postRoll = 5 * time.Second
)

type fakeWriter struct {
	info    clip.OpenInfo
	gops    []*ring.GOP
	final   *clip.FinalInfo
	aborted []clip.EndReason
}

func (w *fakeWriter) WriteGOP(g *ring.GOP) error { w.gops = append(w.gops, g); return nil }
func (w *fakeWriter) Finalize(f clip.FinalInfo) error {
	w.final = &f
	return nil
}
func (w *fakeWriter) Abort(r clip.EndReason) { w.aborted = append(w.aborted, r) }

type fakeOutput struct {
	writers []*fakeWriter
	err     error
}

func (o *fakeOutput) Open(info clip.OpenInfo) (clip.ClipWriter, error) {
	if o.err != nil {
		return nil, o.err
	}
	w := &fakeWriter{info: info}
	o.writers = append(o.writers, w)
	return w, nil
}

type rig struct {
	t        *testing.T
	clk      *clock.Fake
	ring     *ring.Ring
	out      *fakeOutput
	m        *clip.Machine
	sinceIDR int
	started  bool
	gen      uint32
	errs     []error
}

func newRig(t *testing.T) *rig { return newRigMax(t, postRoll, 10*time.Minute) }

func newRigMax(t *testing.T, post, maxClip time.Duration) *rig {
	t.Helper()
	r := &rig{t: t, clk: clock.NewFake(t0), out: &fakeOutput{}, gen: 1}
	r.ring = ring.New(preRoll, 1<<40, nil)
	r.m = clip.New(clip.Config{
		Camera:   "cam1",
		PostRoll: post,
		MaxClip:  maxClip,
		Rand:     rand.Reader,
		OnError:  func(err error) { r.errs = append(r.errs, err) },
	}, r.clk, r.ring, r.out)
	return r
}

// feed delivers frames covering d of media time at 15 fps, advancing the fake
// clock by one frame interval after each.
func (r *rig) feed(d time.Duration) {
	r.t.Helper()
	for n := int(d / step); n > 0; n-- {
		idr := !r.started || r.sinceIDR >= gopLen
		if idr {
			r.sinceIDR = 0
		}
		r.started = true
		r.sinceIDR++
		now := r.clk.Now()
		ticks := now.Sub(t0).Microseconds() * 90 / 1000
		r.m.OnVideo(&camera.AccessUnit{
			PTS: ticks, DTS: ticks,
			IDR: idr, NALUs: [][]byte{{1}}, ParamGen: r.gen, Wall: now, Bytes: 1000,
		})
		r.clk.Advance(step)
	}
}

// forceIDR makes the next fed frame an IDR.
func (r *rig) forceIDR() { r.sinceIDR = gopLen }

func (r *rig) opens() int { return len(r.out.writers) }

func (r *rig) last() *fakeWriter {
	r.t.Helper()
	if len(r.out.writers) == 0 {
		r.t.Fatal("no clip was opened")
	}
	return r.out.writers[len(r.out.writers)-1]
}

// finishWith runs extra, ends the trigger and lets the post-roll elapse.
func (r *rig) finishWith(extra func()) clip.FinalInfo {
	r.t.Helper()
	extra()
	r.m.OnTriggered(false)
	r.feed(postRoll + step)
	r.m.Tick()
	return mustReason(r.t, r.last(), clip.ReasonPostRollElapsed)
}

func mustReason(t *testing.T, w *fakeWriter, want clip.EndReason) clip.FinalInfo {
	t.Helper()
	if w.final == nil {
		t.Fatalf("clip not finalized (aborted=%v)", w.aborted)
	}
	if w.final.Reason != want {
		t.Fatalf("reason = %q, want %q", w.final.Reason, want)
	}
	return *w.final
}

func TestIdleToRecordingOpensAtFirstRetainedGOP(t *testing.T) {
	r := newRig(t)
	r.feed(40 * time.Second)
	if got := r.m.State(); got != "Idle" {
		t.Fatalf("State() = %q before trigger, want Idle", got)
	}
	retained := r.ring.Retained()

	r.m.OnCodec(camera.Codec{Kind: "h264"}, camera.TimeSourceRTCP)
	r.m.OnTriggered(true)

	if r.opens() != 1 {
		t.Fatalf("opens = %d, want 1", r.opens())
	}
	info := r.last().info
	if !info.Start.Equal(retained[0].Start) {
		t.Errorf("Start = %v, want first retained GOP start %v", info.Start, retained[0].Start)
	}
	if got, want := info.ClipID.Time(), ulid.Timestamp(retained[0].Start); got != want {
		t.Errorf("ClipID timestamp = %d, want %d", got, want)
	}
	if info.PreRollMs < 15000 {
		t.Errorf("PreRollMs = %d, want >= 15000", info.PreRollMs)
	}
	if info.SegmentIndex != 0 || info.SeriesID == (ulid.ULID{}) {
		t.Errorf("SegmentIndex=%d SeriesID=%v, want 0 and a series id", info.SegmentIndex, info.SeriesID)
	}
	if info.TimeSource != camera.TimeSourceRTCP || info.Codec.Kind != "h264" {
		t.Errorf("codec/time source = %+v / %q", info.Codec, info.TimeSource)
	}
	if got := r.m.State(); got != "Recording" {
		t.Errorf("State() = %q, want Recording", got)
	}
	// Closed ring GOPs are flushed; the open one is held until it closes.
	if got, want := len(r.last().gops), len(retained)-1; got != want {
		t.Errorf("flushed %d GOPs, want %d", got, want)
	}
}

func TestOpenWaitsForFirstIDR(t *testing.T) {
	r := newRig(t)
	r.m.OnTriggered(true)
	if r.opens() != 0 {
		t.Fatal("opened without any video")
	}
	r.feed(time.Second)
	if r.opens() != 1 {
		t.Fatalf("opens = %d after first IDR, want 1", r.opens())
	}
	if got := r.last().info.PreRollMs; got != 0 {
		t.Errorf("PreRollMs = %d, want 0 for a clip opened on the first IDR", got)
	}
}

func TestRetriggerDuringPostRollCoalesces(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(4 * time.Second)
	r.m.OnTriggered(false)
	if got := r.m.State(); got != "PostRoll" {
		t.Fatalf("State() = %q, want PostRoll", got)
	}
	r.feed(3 * time.Second)
	r.m.OnTriggered(true)
	if got := r.m.State(); got != "Recording" {
		t.Fatalf("State() = %q after re-trigger, want Recording", got)
	}
	r.feed(3 * time.Second) // past the original post-roll deadline
	r.m.Tick()
	if r.opens() != 1 || r.last().final != nil || r.m.State() != "Recording" {
		t.Fatalf("clip ended after coalesced re-trigger: opens=%d final=%v state=%s", r.opens(), r.last().final, r.m.State())
	}
}

func TestPostRollElapsedFinalizes(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(4 * time.Second)
	r.m.OnTriggered(false)
	r.feed(postRoll - time.Second)
	r.m.Tick()
	if r.last().final != nil {
		t.Fatal("finalized before the post-roll elapsed")
	}
	r.feed(time.Second + step)
	r.m.Tick()
	f := mustReason(t, r.last(), clip.ReasonPostRollElapsed)
	if got := r.m.State(); got != "Idle" {
		t.Errorf("State() = %q, want Idle", got)
	}
	start := r.last().info.Start
	if want := f.End.Sub(start).Milliseconds(); f.DurationMs != want {
		t.Errorf("DurationMs = %d, want End-Start = %d", f.DurationMs, want)
	}
	// Every frame fed was written: the last fragment's last frame precedes End.
	last := r.last().gops[len(r.last().gops)-1]
	lastAU := last.AUs[len(last.AUs)-1]
	if !f.End.After(lastAU.Wall) {
		t.Errorf("End %v is not after the last written frame %v", f.End, lastAU.Wall)
	}
	// A new trigger afterwards starts a fresh clip in a fresh series.
	r.m.OnTriggered(true)
	r.m.OnTriggered(false)
	r.m.OnTriggered(true)
	if r.opens() != 2 || r.last().info.SeriesID == r.out.writers[0].info.SeriesID {
		t.Fatalf("expected a second clip in a new series, opens=%d", r.opens())
	}
}

func TestMaxDurationSplitsAtNextIDR(t *testing.T) {
	r := newRigMax(t, postRoll, 20*time.Second)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	first := r.last()
	r.feed(30 * time.Second)

	if r.opens() < 2 {
		t.Fatalf("opens = %d, want a split", r.opens())
	}
	f := mustReason(t, first, clip.ReasonMaxDuration)
	second := r.out.writers[1]
	if second.info.SeriesID != first.info.SeriesID {
		t.Errorf("series differ: %v vs %v", second.info.SeriesID, first.info.SeriesID)
	}
	if second.info.SegmentIndex != 1 || second.info.PreRollMs != 0 {
		t.Errorf("second segment index=%d preroll=%d, want 1 and 0", second.info.SegmentIndex, second.info.PreRollMs)
	}
	if second.info.ClipID == first.info.ClipID {
		t.Error("segments share a ClipID")
	}
	// The split happens at an IDR, which starts the next segment exactly where
	// the first one ends.
	if !second.info.Start.Equal(f.End) {
		t.Errorf("second Start %v != first End %v", second.info.Start, f.End)
	}
	if f.DurationMs < 20000 {
		t.Errorf("first segment duration %d ms, want >= max clip 20000", f.DurationMs)
	}
	if got := r.m.State(); got != "Recording" {
		t.Errorf("State() = %q after split, want Recording", got)
	}
	if got, want := second.info.ClipID.Time(), ulid.Timestamp(second.info.Start); got != want {
		t.Errorf("second ClipID timestamp = %d, want %d", got, want)
	}
	if len(second.gops) > 0 && !second.gops[0].AUs[0].IDR {
		t.Error("second segment does not begin with an IDR")
	}
}

func TestMaxDurationSplitKeepsPostRoll(t *testing.T) {
	r := newRigMax(t, 30*time.Second, 20*time.Second)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.m.OnTriggered(false)
	r.feed(10 * time.Second)
	if r.opens() < 2 {
		t.Fatalf("opens = %d, want a split during post-roll", r.opens())
	}
	if got := r.m.State(); got != "PostRoll" {
		t.Errorf("State() = %q, want PostRoll preserved across the split", got)
	}
}

func TestCodecChangeFinalizesAndReopensWhenTriggered(t *testing.T) {
	r := newRig(t)
	r.m.OnCodec(camera.Codec{Kind: "h264", SPS: []byte{1}}, camera.TimeSourceRTCP)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(5 * time.Second)
	first := r.last()
	prevFrame := r.clk.Now().Add(-step)

	r.m.OnCodec(camera.Codec{Kind: "h264", SPS: []byte{2}}, camera.TimeSourceRTCP)
	f := mustReason(t, first, clip.ReasonCodecChange)
	if f.End.Before(prevFrame) || f.End.After(r.clk.Now()) {
		t.Errorf("End = %v, want at the previous frame %v", f.End, prevFrame)
	}
	if r.m.State() != "Idle" {
		t.Errorf("State() = %q between codec change and next IDR, want Idle", r.m.State())
	}
	r.gen++
	r.forceIDR()
	r.feed(time.Second)

	if r.opens() != 2 {
		t.Fatalf("opens = %d, want reopen on the new codec", r.opens())
	}
	second := r.last()
	if second.info.SeriesID != first.info.SeriesID || second.info.SegmentIndex != 1 || second.info.PreRollMs != 0 {
		t.Errorf("second = series %v idx %d preroll %d", second.info.SeriesID, second.info.SegmentIndex, second.info.PreRollMs)
	}
	if !bytes.Equal(second.info.Codec.SPS, []byte{2}) {
		t.Errorf("second segment codec SPS = %v, want the new one", second.info.Codec.SPS)
	}
	if len(second.gops) != 0 {
		t.Errorf("new segment holds %d closed GOPs from the old codec", len(second.gops))
	}
	if r.m.State() != "Recording" {
		t.Errorf("State() = %q, want Recording", r.m.State())
	}
}

func TestCodecChangeDetectedByParamGen(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(3 * time.Second)
	r.gen++ // parameter sets changed, with no OnCodec call
	r.forceIDR()
	r.feed(time.Second)
	mustReason(t, r.out.writers[0], clip.ReasonCodecChange)
	if r.opens() != 2 {
		t.Fatalf("opens = %d, want 2", r.opens())
	}
}

func TestCodecChangeWithoutTriggerGoesIdle(t *testing.T) {
	r := newRig(t)
	r.m.OnCodec(camera.Codec{Kind: "h264", SPS: []byte{1}}, camera.TimeSourceRTCP)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(3 * time.Second)
	r.m.OnTriggered(false)
	r.m.OnCodec(camera.Codec{Kind: "h264", SPS: []byte{2}}, camera.TimeSourceRTCP)
	mustReason(t, r.last(), clip.ReasonCodecChange)
	r.gen++
	r.forceIDR()
	r.feed(5 * time.Second)
	if r.opens() != 1 || r.m.State() != "Idle" {
		t.Fatalf("opens=%d state=%s, want no reopen when not triggered", r.opens(), r.m.State())
	}
}

func TestLostWithFragmentsFinalizesStreamLost(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(7 * time.Second)
	r.m.OnLost()
	mustReason(t, r.last(), clip.ReasonStreamLost)
	if r.m.State() != "Idle" {
		t.Errorf("State() = %q, want Idle", r.m.State())
	}
	if len(r.ring.Retained()) != 0 {
		t.Error("ring was not reset on loss")
	}
}

func TestLostWithoutFragmentAborts(t *testing.T) {
	r := newRig(t)
	r.m.OnTriggered(true)
	r.feed(time.Second) // opens on the first IDR; its GOP is still open
	r.m.OnLost()
	w := r.last()
	if w.final != nil {
		t.Fatal("clip with no fragment was finalized")
	}
	if !slices.Equal(w.aborted, []clip.EndReason{clip.ReasonStreamLost}) {
		t.Fatalf("aborted = %v, want [stream_lost]", w.aborted)
	}
	if r.m.State() != "Idle" {
		t.Errorf("State() = %q, want Idle", r.m.State())
	}
}

func TestForceFinalizeSuppressesUntilUnsuppress(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(3 * time.Second)

	r.m.ForceFinalize(clip.ReasonBacklogOverflow)
	mustReason(t, r.last(), clip.ReasonBacklogOverflow)
	if r.m.State() != "Suppressed" {
		t.Fatalf("State() = %q, want Suppressed", r.m.State())
	}

	r.m.OnTriggered(false)
	r.m.OnTriggered(true)
	r.feed(5 * time.Second)
	if r.opens() != 1 {
		t.Fatalf("opens = %d while suppressed, want 1", r.opens())
	}

	r.m.Unsuppress()
	if r.m.State() != "Recording" || r.opens() != 2 {
		t.Fatalf("after Unsuppress with trigger held: state=%s opens=%d, want Recording and 2", r.m.State(), r.opens())
	}
	if r.last().info.SeriesID == r.out.writers[0].info.SeriesID {
		t.Error("clip after suppression continued the old series")
	}
}

func TestUnsuppressWithoutTriggerGoesIdle(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.m.ForceFinalize(clip.ReasonBacklogOverflow)
	r.m.OnTriggered(false)
	r.m.Unsuppress()
	if r.m.State() != "Idle" {
		t.Fatalf("State() = %q, want Idle", r.m.State())
	}
}

func TestShutdownFinalizesActiveClip(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	r.feed(3 * time.Second)
	r.m.Shutdown()
	f := mustReason(t, r.last(), clip.ReasonShutdown)
	// Frames of the open GOP are flushed so nothing recorded is lost.
	lastGOP := r.last().gops[len(r.last().gops)-1]
	if lastGOP.AUs[len(lastGOP.AUs)-1].Wall.After(f.End) {
		t.Error("frames written after End")
	}
	r.feed(5 * time.Second)
	r.m.OnTriggered(false)
	r.m.OnTriggered(true)
	if r.opens() != 1 {
		t.Errorf("opens = %d after shutdown, want inputs ignored", r.opens())
	}
}

func TestShutdownWhenIdleIsHarmless(t *testing.T) {
	r := newRig(t)
	r.feed(5 * time.Second)
	r.m.Shutdown()
	if r.opens() != 0 {
		t.Fatal("opened a clip")
	}
}

func TestOpenErrorStaysIdleAndReports(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.out.err = errors.New("backlog full")
	r.m.OnTriggered(true)
	if r.m.State() != "Idle" || len(r.errs) != 1 {
		t.Fatalf("state=%s errs=%v, want Idle and one reported error", r.m.State(), r.errs)
	}
	r.out.err = nil
	r.feed(3 * time.Second) // retried at the next IDR
	if r.opens() != 1 || r.m.State() != "Recording" {
		t.Fatalf("opens=%d state=%s, want retry to succeed", r.opens(), r.m.State())
	}
}

func TestTriggerOffsetsClampedToClip(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	retained := r.ring.Retained()
	// Fires before the first retained GOP (clock skew), still active at open.
	r.m.OnSignal(events.Signal{Kind: events.KindPerson, Active: true, At: retained[0].Start.Add(-5 * time.Second), RawTopic: "t/person"})
	r.m.OnTriggered(true)
	r.feed(3 * time.Second)
	f := r.finishWith(func() {
		// Deactivates long after the clip ended.
		r.m.OnSignal(events.Signal{Kind: events.KindPerson, Active: false, At: r.clk.Now().Add(time.Hour), RawTopic: "t/person"})
	})
	if len(f.Triggers) != 1 {
		t.Fatalf("triggers = %+v, want 1", f.Triggers)
	}
	tr := f.Triggers[0]
	if tr.Kind != events.KindPerson || tr.FirstAtMs != 0 || tr.LastAtMs != f.DurationMs {
		t.Errorf("trigger = %+v, want first 0 and last == duration %d", tr, f.DurationMs)
	}
}

func TestTriggerOffsetsRelativeToClipStart(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	start := r.last().info.Start
	r.m.OnSignal(events.Signal{Kind: events.KindMotion, Active: true, At: start.Add(16 * time.Second), RawTopic: "a"})
	r.m.OnSignal(events.Signal{Kind: events.KindMotion, Active: false, At: start.Add(17 * time.Second), RawTopic: "a"})
	r.feed(4 * time.Second)
	f := r.finishWith(func() {})
	if len(f.Triggers) != 1 || f.Triggers[0].FirstAtMs != 16000 || f.Triggers[0].LastAtMs != 17000 {
		t.Fatalf("triggers = %+v, want motion 16000..17000", f.Triggers)
	}
}

func TestTriggersAggregatePerKindWithDedupedTopics(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnTriggered(true)
	start := r.last().info.Start
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }
	sig := func(k events.Kind, active bool, s float64, topic string) {
		r.m.OnSignal(events.Signal{Kind: k, Active: active, At: at(s), RawTopic: topic})
	}
	const motion, people = "tns1:RuleEngine/CellMotionDetector/Motion", "tns1:RuleEngine/PeopleDetector/People"
	sig(events.KindMotion, true, 16, motion)
	sig(events.KindPerson, true, 16.5, people)
	sig(events.KindMotion, false, 17, motion)
	sig(events.KindMotion, true, 18, motion)
	sig(events.KindMotion, true, 18.5, "tns1:Other/Motion")
	sig(events.KindMotion, false, 19, "tns1:Other/Motion")
	sig(events.KindPerson, false, 19.5, people)
	r.feed(4 * time.Second)
	f := r.finishWith(func() {})

	want := []clip.TriggerRecord{
		{Kind: events.KindMotion, FirstAtMs: 16000, LastAtMs: 19000, RawTopics: []string{motion, "tns1:Other/Motion"}},
		{Kind: events.KindPerson, FirstAtMs: 16500, LastAtMs: 19500, RawTopics: []string{people}},
	}
	if len(f.Triggers) != len(want) {
		t.Fatalf("triggers = %+v, want %+v", f.Triggers, want)
	}
	for i, w := range want {
		g := f.Triggers[i]
		if g.Kind != w.Kind || g.FirstAtMs != w.FirstAtMs || g.LastAtMs != w.LastAtMs || !slices.Equal(g.RawTopics, w.RawTopics) {
			t.Errorf("trigger %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestActiveTriggerCarriesAcrossMaxDurationSplit(t *testing.T) {
	r := newRigMax(t, postRoll, 20*time.Second)
	r.feed(20 * time.Second)
	r.m.OnSignal(events.Signal{Kind: events.KindPerson, Active: true, At: r.clk.Now(), RawTopic: "p"})
	r.m.OnTriggered(true)
	r.feed(30 * time.Second)
	if r.opens() < 2 {
		t.Fatalf("opens = %d, want a split", r.opens())
	}
	f1 := mustReason(t, r.out.writers[0], clip.ReasonMaxDuration)
	if len(f1.Triggers) != 1 || f1.Triggers[0].LastAtMs != f1.DurationMs {
		t.Fatalf("first segment triggers = %+v (duration %d), want person active to the end", f1.Triggers, f1.DurationMs)
	}
	r.m.OnSignal(events.Signal{Kind: events.KindPerson, Active: false, At: r.clk.Now(), RawTopic: "p"})
	r.m.OnTriggered(false)
	r.m.Shutdown()
	f2 := r.last().final
	if f2 == nil || len(f2.Triggers) != 1 || f2.Triggers[0].FirstAtMs != 0 {
		t.Fatalf("continuation final = %+v, want person from offset 0", f2)
	}
}

func TestInactiveSignalsInIdleLeaveNoTrigger(t *testing.T) {
	r := newRig(t)
	r.feed(20 * time.Second)
	r.m.OnSignal(events.Signal{Kind: events.KindMotion, Active: true, At: r.clk.Now(), RawTopic: "m"})
	r.m.OnSignal(events.Signal{Kind: events.KindMotion, Active: false, At: r.clk.Now(), RawTopic: "m"})
	r.m.OnTriggered(true)
	r.feed(3 * time.Second)
	f := r.finishWith(func() {})
	if len(f.Triggers) != 0 {
		t.Fatalf("triggers = %+v, want none from a signal that ended before the clip", f.Triggers)
	}
}
