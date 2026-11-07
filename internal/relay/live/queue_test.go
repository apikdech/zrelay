package live

import (
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

// TestViewerQueueIsPerCamera: viewer_queue_frames can be overridden per
// camera (relay-spec §5.2) and is counted per camera in the memory budget, so
// a viewer of that camera must get that queue, not the global default.
func TestViewerQueueIsPerCamera(t *testing.T) {
	ver, _ := testToken(t)
	s, err := NewServer(Config{
		Listen: "127.0.0.1:0", MaxViewers: 1, Verifier: ver,
		Hubs:         map[string]*camera.Hub{},
		ViewerQueue:  64,
		ViewerQueues: map[string]int{"long-gop": 200, "heavy": 16},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for cam, want := range map[string]int{"long-gop": 200, "heavy": 16, "other": 64} {
		if got := s.viewerQueue(cam); got != want {
			t.Errorf("viewerQueue(%q) = %d, want %d", cam, got, want)
		}
	}
}
