package camera_test

import (
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

var hubCodec = camera.Codec{Kind: "h264", SPS: []byte{0x67, 1}, PPS: []byte{0x68, 2}, Width: 640, Height: 360}

func au(dts int64, idr bool) *camera.AccessUnit {
	return &camera.AccessUnit{PTS: dts, DTS: dts, IDR: idr, NALUs: [][]byte{{0x65}}, Bytes: 1}
}

// recv returns the next event or fails the test if none arrives promptly.
func recv(t *testing.T, s *camera.Sub) camera.Event {
	t.Helper()
	select {
	case ev, ok := <-s.C():
		if !ok {
			t.Fatal("subscriber channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return camera.Event{}
}

// pending returns every event already queued for s, without waiting.
func pending(s *camera.Sub) []camera.Event {
	var evs []camera.Event
	for {
		select {
		case ev, ok := <-s.C():
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		default:
			return evs
		}
	}
}

func TestHubDeliversInOrder(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	s := h.Subscribe(camera.ClassViewer, 16)
	defer s.Close()

	h.Codec(hubCodec, camera.TimeSourceRTCP)
	h.Video(au(0, true))
	h.Audio(&camera.AudioPacket{Codec: "PCMA", Payload: []byte{1}, Seq: 7})
	h.Video(au(3000, false))
	h.Video(au(6000, false))

	ev := recv(t, s)
	if ev.Kind != camera.EventCodec || ev.Codec == nil || !ev.Codec.Equal(hubCodec) || ev.TimeSource != camera.TimeSourceRTCP {
		t.Fatalf("event 0 = %+v, want codec", ev)
	}
	ev = recv(t, s)
	if ev.Kind != camera.EventVideo || ev.AU == nil || ev.AU.DTS != 0 || !ev.AU.IDR {
		t.Fatalf("event 1 = %+v, want IDR video at 0", ev)
	}
	ev = recv(t, s)
	if ev.Kind != camera.EventAudio || ev.Audio == nil || ev.Audio.Seq != 7 {
		t.Fatalf("event 2 = %+v, want audio seq 7", ev)
	}
	for _, want := range []int64{3000, 6000} {
		ev = recv(t, s)
		if ev.Kind != camera.EventVideo || ev.AU.DTS != want {
			t.Fatalf("event = %+v, want video at %d", ev, want)
		}
	}
	h.Lost()
	if ev = recv(t, s); ev.Kind != camera.EventLost {
		t.Fatalf("after Lost: %+v, want EventLost", ev)
	}
	if got := s.Lagged(); got != 0 {
		t.Fatalf("Lagged = %d, want 0", got)
	}
	if rest := pending(s); len(rest) != 0 {
		t.Fatalf("unexpected extra events %+v", rest)
	}
}

func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	t.Parallel()
	const n = 10000
	h := camera.NewHub("front")
	slow := h.Subscribe(camera.ClassRecorder, 8) // never read
	defer slow.Close()
	fast := h.Subscribe(camera.ClassViewer, n+16)
	defer fast.Close()

	h.Codec(hubCodec, camera.TimeSourceArrival)
	start := time.Now()
	for i := range n {
		h.Video(au(int64(i)*3000, i%50 == 0))
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("%d publishes took %v, want <= 100ms", n, d)
	}

	if ev := recv(t, fast); ev.Kind != camera.EventCodec {
		t.Fatalf("fast first event = %+v, want codec", ev)
	}
	for i := range n {
		ev := recv(t, fast)
		if ev.Kind != camera.EventVideo || ev.AU.DTS != int64(i)*3000 {
			t.Fatalf("fast event %d = %+v, want video at %d", i, ev, int64(i)*3000)
		}
	}
	if fast.Lagged() != 0 {
		t.Fatalf("fast Lagged = %d, want 0", fast.Lagged())
	}
	if slow.Lagged() == 0 {
		t.Fatal("slow subscriber never marked lagged")
	}
}

func TestLaggedSubscriberResumesAtIDR(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	s := h.Subscribe(camera.ClassRecorder, 4)
	defer s.Close()

	h.Codec(hubCodec, camera.TimeSourceRTCP)
	h.Video(au(0, true))
	for i := 1; i <= 20; i++ { // overflows the queue of 4
		h.Video(au(int64(i)*3000, false))
	}
	if s.Lagged() == 0 {
		t.Fatal("subscriber not marked lagged")
	}
	h.Video(au(21*3000, true))
	h.Video(au(22*3000, false))

	lost := 0
	for {
		ev := recv(t, s)
		if ev.Kind == camera.EventLost {
			lost++
			continue
		}
		if ev.Kind != camera.EventVideo {
			continue
		}
		if lost != 1 {
			t.Fatalf("saw %d EventLost before resuming, want exactly 1", lost)
		}
		if !ev.AU.IDR || ev.AU.DTS != 21*3000 {
			t.Fatalf("first video after lag = DTS %d IDR %v, want the IDR at %d", ev.AU.DTS, ev.AU.IDR, 21*3000)
		}
		break
	}
	if ev := recv(t, s); ev.Kind != camera.EventVideo || ev.AU.DTS != 22*3000 {
		t.Fatalf("after resync: %+v, want video at %d", ev, 22*3000)
	}
}

// After a lag the subscriber still knows the codec: the drained queue may
// have held the only EventCodec it would have seen.
func TestLaggedSubscriberGetsCodecAgain(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	s := h.Subscribe(camera.ClassRecorder, 4)
	defer s.Close()

	h.Codec(hubCodec, camera.TimeSourceRTCP)
	for i := range 10 {
		h.Video(au(int64(i)*3000, i == 0))
	}
	h.Video(au(30000, true))

	var kinds []camera.EventKind
	var codec *camera.Codec
	for _, ev := range pending(s) {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == camera.EventCodec {
			codec = ev.Codec
		}
	}
	want := []camera.EventKind{camera.EventLost, camera.EventCodec, camera.EventVideo}
	if len(kinds) != len(want) {
		t.Fatalf("events after lag = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events after lag = %v, want %v", kinds, want)
		}
	}
	if codec == nil || !codec.Equal(hubCodec) {
		t.Fatalf("codec after lag = %+v, want %+v", codec, hubCodec)
	}
}

func TestLaggedViewerGetsAudioBeforeIDR(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	viewer := h.Subscribe(camera.ClassViewer, 4)
	defer viewer.Close()

	h.Codec(hubCodec, camera.TimeSourceRTCP)
	for i := range 10 {
		h.Video(au(int64(i)*3000, i == 0))
	}
	pending(viewer) // Lost and Codec
	if viewer.Lagged() == 0 {
		t.Fatal("viewer not marked lagged")
	}
	h.Video(au(30000, false))
	h.Audio(&camera.AudioPacket{Codec: "PCMU", Seq: 1})
	ev := recv(t, viewer)
	if ev.Kind != camera.EventAudio || ev.Audio.Seq != 1 {
		t.Fatalf("viewer got %+v, want the audio packet (and no P frame)", ev)
	}
}

func TestRecorderGetsNoAudio(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	rec := h.Subscribe(camera.ClassRecorder, 16)
	defer rec.Close()

	h.Codec(hubCodec, camera.TimeSourceRTCP)
	h.Video(au(0, true))
	h.Audio(&camera.AudioPacket{Codec: "PCMA", Seq: 1})
	h.Video(au(3000, false))
	for _, ev := range pending(rec) {
		if ev.Kind == camera.EventAudio {
			t.Fatal("recorder received audio (design §4.1: audio never reaches the recorder)")
		}
	}
}

func TestLateSubscriberGetsCurrentCodec(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	if _, _, ok := h.CurrentCodec(); ok {
		t.Fatal("CurrentCodec ok before any codec")
	}
	h.Codec(camera.Codec{Kind: "h264", SPS: []byte{1}, PPS: []byte{2}}, camera.TimeSourceArrival)
	h.Codec(hubCodec, camera.TimeSourceRTCP)
	h.Video(au(0, true))
	h.Video(au(3000, false))

	c, ts, ok := h.CurrentCodec()
	if !ok || !c.Equal(hubCodec) || ts != camera.TimeSourceRTCP {
		t.Fatalf("CurrentCodec() = %+v %q %v, want %+v rtcp true", c, ts, ok, hubCodec)
	}

	s := h.Subscribe(camera.ClassViewer, 16)
	defer s.Close()
	ev := recv(t, s)
	if ev.Kind != camera.EventCodec || !ev.Codec.Equal(hubCodec) || ev.TimeSource != camera.TimeSourceRTCP {
		t.Fatalf("late subscriber first event = %+v, want current codec", ev)
	}
	// Joining mid-GOP: video starts at the next IDR.
	h.Video(au(6000, false))
	h.Video(au(9000, true))
	if ev := recv(t, s); ev.Kind != camera.EventVideo || !ev.AU.IDR || ev.AU.DTS != 9000 {
		t.Fatalf("late subscriber first video = %+v, want the IDR at 9000", ev)
	}

	h.Lost()
	if _, _, ok := h.CurrentCodec(); ok {
		t.Fatal("CurrentCodec ok after Lost; the session that set it is gone")
	}
}

func TestCloseUnsubscribes(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	s := h.Subscribe(camera.ClassViewer, 4)
	other := h.Subscribe(camera.ClassViewer, 64)
	defer other.Close()

	h.Codec(hubCodec, camera.TimeSourceRTCP)
	s.Close()
	s.Close() // idempotent

	// The channel is closed once the queued events are read.
	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-s.C():
		case <-deadline:
			t.Fatal("channel not closed after Close")
		}
	}
	// Publishing after Close neither panics nor reaches s.
	h.Video(au(0, true))
	h.Audio(&camera.AudioPacket{Codec: "PCMA"})
	h.Lost()
	if s.Lagged() != 0 {
		t.Fatalf("closed subscriber Lagged = %d, want 0", s.Lagged())
	}
	if got := len(pending(other)); got != 4 {
		t.Fatalf("other subscriber got %d events, want 4", got)
	}
}

// Subscribe, Close and publishing race freely (run with -race).
func TestHubConcurrentSubscribeClose(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 2000 {
			s := h.Subscribe(camera.Class(i%2), 2)
			pending(s)
			s.Close()
		}
	}()
	h.Codec(hubCodec, camera.TimeSourceRTCP)
	for i := 0; ; i++ {
		select {
		case <-done:
			return
		default:
		}
		h.Video(au(int64(i)*3000, i%10 == 0))
		h.Audio(&camera.AudioPacket{Codec: "PCMA"})
		if i%500 == 0 {
			h.Lost()
			h.Codec(hubCodec, camera.TimeSourceRTCP)
		}
	}
}

var _ camera.SessionSink = (*camera.Hub)(nil)

// Camera names the Hub, for per-camera metrics such as
// zrelay_recorder_lag_total{camera}.
func TestHubCamera(t *testing.T) {
	t.Parallel()
	if got := camera.NewHub("front").Camera(); got != "front" {
		t.Fatalf("Camera() = %q, want %q", got, "front")
	}
}

// A session loss that finds a subscriber's queue full is not a lag: the
// subscriber gets exactly one EventLost (its stale queue cleared), and
// Lagged, which feeds zrelay_recorder_lag_total, is unchanged.
func TestLostOnFullQueueIsNotLag(t *testing.T) {
	t.Parallel()
	h := camera.NewHub("front")
	s := h.Subscribe(camera.ClassRecorder, 2)
	defer s.Close()

	h.Codec(hubCodec, camera.TimeSourceRTCP)
	h.Video(au(0, true)) // queue now full: codec, IDR
	h.Lost()

	evs := pending(s)
	if len(evs) != 1 || evs[0].Kind != camera.EventLost {
		t.Fatalf("events after Lost = %+v, want exactly one EventLost", evs)
	}
	if got := s.Lagged(); got != 0 {
		t.Fatalf("Lagged = %d, want 0 (a session loss is not recorder lag)", got)
	}
	// Video still resumes at the next IDR.
	h.Codec(hubCodec, camera.TimeSourceRTCP)
	h.Video(au(3000, false))
	h.Video(au(6000, true))
	evs = pending(s)
	if len(evs) != 2 || evs[0].Kind != camera.EventCodec || evs[1].Kind != camera.EventVideo || !evs[1].AU.IDR {
		t.Fatalf("events after reconnect = %+v, want codec then IDR", evs)
	}
}
