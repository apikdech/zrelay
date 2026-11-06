package live

import (
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

func TestViewerSkipsNonH264Video(t *testing.T) {
	hub := camera.NewHub("cam1")
	hub.Codec(camera.Codec{Kind: "h265"}, camera.TimeSourceArrival)
	sub := hub.Subscribe(camera.ClassViewer, 16)
	w := newGatedWriter()
	close(w.gate)
	defer runViewer(newViewer(sub, w, nil, ""), sub)()

	hub.Video(mkAU(true, 0, 0xa1))
	hub.Video(mkAU(false, 6000, 0xa2))
	// Once the queue is empty the viewer has consumed everything; give it
	// time to finish the last event.
	eventually(t, 3*time.Second, "queue drained", func() bool { return len(sub.C()) == 0 })
	time.Sleep(50 * time.Millisecond)
	if got := w.snapshot(); len(got) != 0 {
		t.Fatalf("H.265 access units were written: %d samples", len(got))
	}
}

func TestViewerAudioDurationResetsAfterLoss(t *testing.T) {
	hub := camera.NewHub("cam1")
	sub := hub.Subscribe(camera.ClassViewer, 16)
	vw, aw := newGatedWriter(), newGatedWriter()
	close(vw.gate)
	close(aw.gate)
	defer runViewer(newViewer(sub, vw, aw, "PCMA"), sub)()

	hub.Audio(&camera.AudioPacket{Codec: "PCMA", Payload: []byte{1}, Timestamp: 1000})
	hub.Lost()
	// The first packet of the new session must not be timed against the
	// previous session's timestamp (4000 ticks would be 500 ms).
	hub.Audio(&camera.AudioPacket{Codec: "PCMA", Payload: []byte{2}, Timestamp: 5000})
	eventually(t, 3*time.Second, "audio samples", func() bool { return len(aw.snapshot()) >= 2 })
	got := aw.snapshot()
	if got[1].Duration != defaultAudioDuration {
		t.Fatalf("first sample after loss lasts %v, want %v", got[1].Duration, defaultAudioDuration)
	}
}
