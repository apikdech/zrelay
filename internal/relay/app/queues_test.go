package app_test

import (
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
)

// TestViewerQueuesFollowThePerCameraOverride: config check budgets each
// camera's viewer queue from its own viewer_queue_frames, so the running
// relay must subscribe live view and restream with that same value.
func TestViewerQueuesFollowThePerCameraOverride(t *testing.T) {
	r := newRig(t, "")
	r.cfg.Defaults.ViewerQueueFrames = 64
	r.cfg.Cameras[0].ViewerQueueFrames = 200
	second := r.cfg.Cameras[0]
	second.ID = "cam-2"
	second.ViewerQueueFrames = 64
	r.cfg.Cameras = append(r.cfg.Cameras, second)

	got := app.ViewerQueues(r.cfg)
	if got["cam-1"] != 200 || got["cam-2"] != 64 || len(got) != 2 {
		t.Errorf("ViewerQueues = %v, want cam-1:200 cam-2:64", got)
	}
}
