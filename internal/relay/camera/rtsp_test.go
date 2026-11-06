package camera_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const (
	h264Fixture = "../../../testdata/video/h264-640x360-gop2-10s.h264"
	h265Fixture = "../../../testdata/video/h265-640x360-gop2-5s.h265"
	ticksPerAU  = 90000 / 15
)

// event is one sink call, in order.
type event struct {
	codec *camera.Codec
	ts    camera.TimeSource
	video *camera.AccessUnit
	audio *camera.AudioPacket
}

// recSink records every SessionSink call.
type recSink struct {
	mu     sync.Mutex
	events []event
	ch     chan struct{}
}

func newRecSink() *recSink { return &recSink{ch: make(chan struct{}, 1)} }

func (s *recSink) add(e event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

func (s *recSink) Codec(c camera.Codec, ts camera.TimeSource) { s.add(event{codec: &c, ts: ts}) }
func (s *recSink) Video(au *camera.AccessUnit)                { s.add(event{video: au}) }
func (s *recSink) Audio(p *camera.AudioPacket)                { s.add(event{audio: p}) }

func (s *recSink) snapshot() []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event(nil), s.events...)
}

func videos(evs []event) []*camera.AccessUnit {
	var out []*camera.AccessUnit
	for _, e := range evs {
		if e.video != nil {
			out = append(out, e.video)
		}
	}
	return out
}

func codecs(evs []event) []event {
	var out []event
	for _, e := range evs {
		if e.codec != nil {
			out = append(out, e)
		}
	}
	return out
}

func audios(evs []event) []*camera.AudioPacket {
	var out []*camera.AudioPacket
	for _, e := range evs {
		if e.audio != nil {
			out = append(out, e.audio)
		}
	}
	return out
}

// waitFor blocks until cond holds on the recorded events or limit passes.
func (s *recSink) waitFor(t *testing.T, limit time.Duration, what string, cond func([]event) bool) []event {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		if evs := s.snapshot(); cond(evs) {
			return evs
		}
		select {
		case <-s.ch:
		case <-deadline.C:
			evs := s.snapshot()
			t.Fatalf("timed out after %v waiting for %s (%d events, %d AUs)", limit, what, len(evs), len(videos(evs)))
		}
	}
}

func atLeastAUs(n int) func([]event) bool {
	return func(evs []event) bool { return len(videos(evs)) >= n }
}

// run starts RunRTSP in the background. stop cancels it and returns its
// error; done is closed when it returns, after which *errp holds the error.
func run(t *testing.T, url string, sink camera.SessionSink) (stop func() error, done <-chan struct{}, errp *error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var err error
	go func() {
		defer close(finished)
		err = camera.RunRTSP(ctx, url, sink)
	}()
	stop = func() error {
		cancel()
		select {
		case <-finished:
			return err
		case <-time.After(15 * time.Second):
			t.Errorf("RunRTSP did not return after cancel")
			return nil
		}
	}
	t.Cleanup(func() { stop() })
	return stop, finished, &err
}

func checkDTSStrictlyIncreasing(t *testing.T, aus []*camera.AccessUnit) {
	t.Helper()
	for i := 1; i < len(aus); i++ {
		if aus[i].DTS <= aus[i-1].DTS {
			t.Fatalf("AU %d: DTS %d not after %d", i, aus[i].DTS, aus[i-1].DTS)
		}
	}
}

func TestRunRTSPDeliversAccessUnits(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15})
	sink := newRecSink()
	stop, _, _ := run(t, srv.URL(), sink)

	evs := sink.waitFor(t, 20*time.Second, "140 access units", atLeastAUs(140))
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("RunRTSP after cancel = %v, want context.Canceled", err)
	}

	if evs[0].codec == nil {
		t.Fatal("first sink call is not Codec")
	}
	if n := len(codecs(evs)); n != 1 {
		t.Fatalf("Codec called %d times, want 1", n)
	}
	c := *evs[0].codec
	if c.Kind != "h264" || len(c.SPS) == 0 || len(c.PPS) == 0 || c.Width != 640 || c.Height != 360 {
		t.Fatalf("codec = %+v", c)
	}
	got := videos(evs)
	if !got[0].IDR {
		t.Fatal("first AU is not IDR")
	}
	if got[0].DTS != 0 || got[0].PTS != 0 {
		t.Fatalf("first AU PTS/DTS = %d/%d, want 0/0", got[0].PTS, got[0].DTS)
	}
	checkDTSStrictlyIncreasing(t, got)
	idrs := 0
	for i, au := range got {
		if au.ParamGen != 0 {
			t.Fatalf("AU %d ParamGen = %d", i, au.ParamGen)
		}
		if au.IDR {
			idrs++
		}
		n := 0
		for _, nalu := range au.NALUs {
			n += len(nalu)
		}
		if au.Bytes != n || n == 0 {
			t.Fatalf("AU %d Bytes = %d, NALU bytes %d", i, au.Bytes, n)
		}
		if au.Wall.IsZero() || au.Wall.Location() != time.UTC {
			t.Fatalf("AU %d Wall = %v, want UTC", i, au.Wall)
		}
	}
	if idrs < 4 {
		t.Fatalf("%d IDRs in %d AUs, want >= 4 (GOP 2 s)", idrs, len(got))
	}
}

func TestParamGenBumpsOnSPSChange(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15})
	sink := newRecSink()
	run(t, srv.URL(), sink)

	evs := sink.waitFor(t, 10*time.Second, "first AUs", atLeastAUs(5))
	orig := *codecs(evs)[0].codec
	newSPS := bytes.Clone(orig.SPS)
	newSPS[3]++ // level_idc: a different SPS that parses the same way
	srv.SetParamSets(newSPS, orig.PPS)

	hasGen1 := func(evs []event) bool {
		for _, au := range videos(evs) {
			if au.ParamGen == 1 {
				return true
			}
		}
		return false
	}
	sink.waitFor(t, 10*time.Second, "AU with ParamGen 1", hasGen1)
	evs = sink.waitFor(t, 10*time.Second, "AUs after the change", func(evs []event) bool {
		n := 0
		for _, au := range videos(evs) {
			if au.ParamGen == 1 {
				n++
			}
		}
		return n >= 5
	})

	cs := codecs(evs)
	if len(cs) != 2 {
		t.Fatalf("Codec called %d times, want 2", len(cs))
	}
	if !bytes.Equal(cs[1].codec.SPS, newSPS) {
		t.Fatalf("second Codec SPS = %x, want %x", cs[1].codec.SPS, newSPS)
	}
	if cs[1].codec.Equal(orig) || !cs[1].codec.Equal(*cs[1].codec) {
		t.Fatal("Codec.Equal does not tell the parameter sets apart")
	}
	// Order: every gen-1 AU follows the second Codec call, which follows
	// every gen-0 AU; the first gen-1 AU is an IDR.
	seenSecond := false
	first := true
	for _, e := range evs {
		switch {
		case e.codec != nil && e.codec.Equal(*cs[1].codec):
			seenSecond = true
		case e.video != nil && e.video.ParamGen == 0:
			if seenSecond {
				t.Fatal("ParamGen 0 AU after the second Codec call")
			}
		case e.video != nil && e.video.ParamGen == 1:
			if !seenSecond {
				t.Fatal("ParamGen 1 AU before the second Codec call")
			}
			if first && !e.video.IDR {
				t.Fatal("first ParamGen 1 AU is not IDR")
			}
			first = false
		}
	}
	checkDTSStrictlyIncreasing(t, videos(evs))
}

func TestAccessUnitsMonotonicAcrossRTPWrap(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	// 0x10000 ticks before the wrap: about 11 AUs at 15 fps.
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, StartRTPTimestamp: 0xFFFF_0000})
	sink := newRecSink()
	run(t, srv.URL(), sink)

	got := videos(sink.waitFor(t, 10*time.Second, "30 AUs", atLeastAUs(30)))
	checkDTSStrictlyIncreasing(t, got)
	for i := 1; i < len(got); i++ {
		if d := got[i].DTS - got[i-1].DTS; d != ticksPerAU {
			t.Fatalf("AU %d: DTS step %d, want %d", i, d, ticksPerAU)
		}
		if d := got[i].Wall.Sub(got[i-1].Wall); d < 0 || d > time.Second {
			t.Fatalf("AU %d: Wall step %v", i, d)
		}
	}
	if last := got[len(got)-1].DTS; last != int64(len(got)-1)*ticksPerAU {
		t.Fatalf("last DTS %d, want %d", last, int64(len(got)-1)*ticksPerAU)
	}
}

func TestWallClockUsesRTCPWhenPresent(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, RTCP: true})
	sink := newRecSink()
	start := time.Now()
	run(t, srv.URL(), sink)

	evs := sink.waitFor(t, 10*time.Second, "15 AUs", atLeastAUs(15))
	cs := codecs(evs)
	if cs[0].ts != camera.TimeSource("rtcp") {
		t.Fatalf("time source = %q, want rtcp", cs[0].ts)
	}
	checkWall(t, videos(evs), start)
}

func TestWallClockFallsBackToArrival(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15})
	sink := newRecSink()
	start := time.Now()
	run(t, srv.URL(), sink)

	evs := sink.waitFor(t, 10*time.Second, "15 AUs", atLeastAUs(15))
	cs := codecs(evs)
	if cs[0].ts != camera.TimeSource("arrival") {
		t.Fatalf("time source = %q, want arrival", cs[0].ts)
	}
	got := videos(evs)
	checkWall(t, got, start)
	// Arrival mode: Wall is the first packet's arrival plus the DTS offset.
	for i, au := range got {
		want := got[0].Wall.Add(time.Duration(au.DTS-got[0].DTS) * time.Second / 90000)
		if !au.Wall.Equal(want) {
			t.Fatalf("AU %d: Wall %v, want %v", i, au.Wall, want)
		}
	}
}

// checkWall checks Wall is UTC, near the test's start and paced by DTS.
func checkWall(t *testing.T, got []*camera.AccessUnit, start time.Time) {
	t.Helper()
	if got[0].Wall.Location() != time.UTC {
		t.Fatalf("Wall %v is not UTC", got[0].Wall)
	}
	if d := got[0].Wall.Sub(start); d < -2*time.Second || d > 5*time.Second {
		t.Fatalf("first Wall %v is %v from the test start", got[0].Wall, d)
	}
	for i := 1; i < len(got); i++ {
		wall := got[i].Wall.Sub(got[0].Wall)
		media := time.Duration(got[i].DTS-got[0].DTS) * time.Second / 90000
		if d := wall - media; d < -50*time.Millisecond || d > 50*time.Millisecond {
			t.Fatalf("AU %d: Wall offset %v, DTS offset %v", i, wall, media)
		}
	}
}

func TestRunRTSPErrorRedactsCredentials(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := camera.RunRTSP(ctx, "rtsp://u:s3cret@127.0.0.1:1/x", newRecSink())
	if err == nil {
		t.Fatal("RunRTSP to a closed port returned nil")
	}
	if msg := err.Error(); strings.Contains(msg, "s3cret") || strings.Contains(msg, "u:s3cret") {
		t.Fatalf("error leaks credentials: %q", msg)
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()
	const raw = "rtsp://admin:p%40ss@cam.local:554/stream1"
	err := camera.Redact(errors.New("dial rtsp://admin:p%40ss@cam.local:554 failed, password p@ss"), raw)
	if msg := err.Error(); strings.Contains(msg, "p@ss") || strings.Contains(msg, "p%40ss") || strings.Contains(msg, "admin:") {
		t.Fatalf("Redact left credentials: %q", msg)
	}
	if camera.Redact(nil, raw) != nil {
		t.Fatal("Redact(nil) != nil")
	}
	plain := errors.New("connection refused")
	if got := camera.Redact(plain, raw); got != plain {
		t.Fatalf("Redact changed an error without credentials: %v", got)
	}
	if got := camera.Redact(context.Canceled, raw); !errors.Is(got, context.Canceled) {
		t.Fatalf("Redact(context.Canceled) = %v, lost the chain", got)
	}
}

func TestRunRTSPReturnsOnSessionLoss(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, Loop: true})
	sink := newRecSink()
	_, done, errp := run(t, srv.URL(), sink)
	sink.waitFor(t, 10*time.Second, "first AU", atLeastAUs(1))
	srv.Disconnect()
	select {
	case <-done:
		if err := *errp; err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("RunRTSP after session loss = %v, want a session error", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunRTSP did not return after session loss")
	}
}

func TestH265Accepted(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h265Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h265", AUs: aus, FPS: 15})
	sink := newRecSink()
	run(t, srv.URL(), sink)

	evs := sink.waitFor(t, 10*time.Second, "40 H.265 AUs", atLeastAUs(40))
	cs := codecs(evs)
	if len(cs) != 1 || evs[0].codec == nil {
		t.Fatalf("Codec calls = %d, first event codec = %v", len(cs), evs[0].codec != nil)
	}
	c := *cs[0].codec
	if c.Kind != "h265" || len(c.VPS) == 0 || len(c.SPS) == 0 || len(c.PPS) == 0 || c.Width != 640 || c.Height != 360 {
		t.Fatalf("codec = %+v", c)
	}
	got := videos(evs)
	if !got[0].IDR || got[0].DTS != 0 {
		t.Fatalf("first AU IDR=%v DTS=%d", got[0].IDR, got[0].DTS)
	}
	checkDTSStrictlyIncreasing(t, got)
}

func TestAudioPCMADelivered(t *testing.T) {
	t.Parallel()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, Audio: true})
	sink := newRecSink()
	run(t, srv.URL(), sink)

	evs := sink.waitFor(t, 10*time.Second, "20 audio packets", func(evs []event) bool { return len(audios(evs)) >= 20 })
	if evs[0].codec == nil {
		t.Fatal("first sink call is not Codec")
	}
	as := audios(evs)
	for i, p := range as {
		if p.Codec != "PCMA" {
			t.Fatalf("audio %d codec %q, want PCMA", i, p.Codec)
		}
		if len(p.Payload) != 160 || p.Payload[0] != 0xd5 {
			t.Fatalf("audio %d payload len %d", i, len(p.Payload))
		}
		if i > 0 {
			if p.Seq != as[i-1].Seq+1 {
				t.Fatalf("audio %d seq %d after %d", i, p.Seq, as[i-1].Seq)
			}
			if p.Timestamp != as[i-1].Timestamp+160 {
				t.Fatalf("audio %d timestamp %d after %d", i, p.Timestamp, as[i-1].Timestamp)
			}
		}
	}
}

func TestNoKeyframeNoVideo(t *testing.T) {
	t.Parallel()
	all := testutil.LoadAnnexB(t, h264Fixture)
	if !all[30].IDR {
		t.Fatal("fixture AU 30 is not IDR")
	}
	// Start mid-GOP: AUs 5..29 have no IDR and must be dropped.
	aus := all[5:]
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15})
	sink := newRecSink()
	run(t, srv.URL(), sink)

	evs := sink.waitFor(t, 10*time.Second, "AUs after the first IDR", atLeastAUs(15))
	got := videos(evs)
	if !got[0].IDR || got[0].DTS != 0 {
		t.Fatalf("first AU IDR=%v DTS=%d", got[0].IDR, got[0].DTS)
	}
	for i, au := range got {
		// The AUs from the first IDR on are all[30:], in order.
		if !bytes.Equal(au.NALUs[len(au.NALUs)-1], all[30+i].NALUs[len(all[30+i].NALUs)-1]) {
			t.Fatalf("AU %d is not fixture AU %d", i, 30+i)
		}
	}
}
