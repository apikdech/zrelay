package restream

import (
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

// TestViewerQueueIsPerCamera: a camera's viewer_queue_frames override sizes
// that camera's restream subscription (relay-spec §5.2), the default the rest.
func TestViewerQueueIsPerCamera(t *testing.T) {
	s, err := New(Config{
		Listen:       "127.0.0.1:0",
		Username:     "u",
		Password:     "p",
		Hubs:         map[string]*camera.Hub{},
		ViewerQueue:  64,
		ViewerQueues: map[string]int{"long-gop": 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for cam, want := range map[string]int{"long-gop": 200, "other": 64} {
		if got := s.viewerQueue(cam); got != want {
			t.Errorf("viewerQueue(%q) = %d, want %d", cam, got, want)
		}
	}
}
