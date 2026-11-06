package live

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

// gatedWriter records samples; the first WriteSample blocks until released.
type gatedWriter struct {
	mu      sync.Mutex
	samples []media.Sample
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{gate: make(chan struct{}), started: make(chan struct{})}
}

func (w *gatedWriter) WriteSample(s media.Sample) error {
	w.once.Do(func() {
		close(w.started)
		<-w.gate
	})
	w.mu.Lock()
	w.samples = append(w.samples, s)
	w.mu.Unlock()
	return nil
}

func (w *gatedWriter) snapshot() []media.Sample {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]media.Sample(nil), w.samples...)
}

func mkAU(idr bool, dts int64, marker byte) *camera.AccessUnit {
	typ := byte(0x41)
	if idr {
		typ = 0x65
	}
	return &camera.AccessUnit{DTS: dts, PTS: dts, IDR: idr, NALUs: [][]byte{{typ, marker, marker}}, Bytes: 3}
}

func runViewer(v *viewer, sub *camera.Sub) (stop func()) {
	done := make(chan struct{})
	go func() { v.run(); close(done) }()
	return func() { sub.Close(); <-done }
}

func TestViewerLagResyncsAtIDR(t *testing.T) {
	hub := camera.NewHub("cam1")
	hub.Codec(camera.Codec{Kind: "h264", SPS: []byte{0x67, 1, 2}, PPS: []byte{0x68, 3}}, camera.TimeSourceArrival)
	sub := hub.Subscribe(camera.ClassViewer, 1)
	w := newGatedWriter()
	defer runViewer(newViewer(sub, w, nil, ""), sub)()

	hub.Video(mkAU(true, 0, 0xa1))
	<-w.started // the writer is now stuck on the first IDR
	for i := int64(1); sub.Lagged() == 0; i++ {
		if i > 100 {
			t.Fatal("subscriber never lagged")
		}
		hub.Video(mkAU(false, i*6000, 0xb0))
	}
	hub.Video(mkAU(false, 200000, 0xb1)) // skipped: waiting for IDR
	close(w.gate)
	// A queue of 2 holds only the EventLost and EventCodec the lag left;
	// let the viewer drain them before the camera sends its next IDR.
	eventually(t, 3*time.Second, "viewer to drain the lag events", func() bool { return len(sub.C()) == 0 })
	hub.Video(mkAU(false, 204000, 0xb2)) // still skipped: waiting for IDR
	hub.Video(mkAU(true, 210000, 0xc1))
	eventually(t, 3*time.Second, "IDR written", func() bool { return len(w.snapshot()) >= 2 })
	hub.Video(mkAU(false, 216000, 0xd1))

	eventually(t, 3*time.Second, "samples after resync", func() bool { return len(w.snapshot()) >= 3 })
	got := w.snapshot()
	if !bytes.Contains(got[0].Data, []byte{0x65, 0xa1, 0xa1}) {
		t.Fatalf("first sample is not the first IDR: %x", got[0].Data)
	}
	// The next sample written after the lag is the new IDR, not a P frame.
	if !bytes.Contains(got[1].Data, []byte{0x65, 0xc1, 0xc1}) {
		t.Fatalf("sample after lag is not IDR c1: %x", got[1].Data)
	}
	if !bytes.Contains(got[2].Data, []byte{0x41, 0xd1, 0xd1}) {
		t.Fatalf("third sample %x", got[2].Data)
	}
}

func TestViewerSamplesAreAnnexBWithParamSetsAndDTSDurations(t *testing.T) {
	hub := camera.NewHub("cam1")
	hub.Codec(camera.Codec{Kind: "h264", SPS: []byte{0x67, 1, 2}, PPS: []byte{0x68, 3}}, camera.TimeSourceArrival)
	sub := hub.Subscribe(camera.ClassViewer, 16)
	w := newGatedWriter()
	close(w.gate)
	defer runViewer(newViewer(sub, w, nil, ""), sub)()

	hub.Video(mkAU(true, 0, 0xa1))
	hub.Video(mkAU(false, 6000, 0xa2))
	hub.Video(mkAU(false, 15000, 0xa3))
	eventually(t, 3*time.Second, "samples", func() bool { return len(w.snapshot()) >= 3 })
	got := w.snapshot()
	want := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 0xa1, 0xa1}
	if !bytes.Equal(got[0].Data, want) {
		t.Fatalf("IDR sample = %x, want %x", got[0].Data, want)
	}
	if !bytes.Equal(got[1].Data, []byte{0, 0, 0, 1, 0x41, 0xa2, 0xa2}) {
		t.Fatalf("P sample = %x", got[1].Data)
	}
	if got[1].Duration != time.Second*6000/90000 || got[2].Duration != time.Second*9000/90000 {
		t.Fatalf("durations %v %v, want 66.6ms 100ms", got[1].Duration, got[2].Duration)
	}
}

func TestViewerAudioSamples(t *testing.T) {
	hub := camera.NewHub("cam1")
	sub := hub.Subscribe(camera.ClassViewer, 16)
	vw, aw := newGatedWriter(), newGatedWriter()
	close(vw.gate)
	close(aw.gate)
	defer runViewer(newViewer(sub, vw, aw, "PCMA"), sub)()

	hub.Audio(&camera.AudioPacket{Codec: "PCMA", Payload: []byte{1, 2}, Timestamp: 1000})
	hub.Audio(&camera.AudioPacket{Codec: "PCMU", Payload: []byte{9}, Timestamp: 1160}) // wrong codec: dropped
	hub.Audio(&camera.AudioPacket{Codec: "PCMA", Payload: []byte{3, 4}, Timestamp: 1320})
	eventually(t, 3*time.Second, "audio samples", func() bool { return len(aw.snapshot()) >= 2 })
	got := aw.snapshot()
	if len(got) != 2 || !bytes.Equal(got[0].Data, []byte{1, 2}) || !bytes.Equal(got[1].Data, []byte{3, 4}) {
		t.Fatalf("audio samples %+v", got)
	}
	if got[1].Duration != 40*time.Millisecond {
		t.Fatalf("duration %v, want 40ms (320 ticks at 8 kHz)", got[1].Duration)
	}
}
