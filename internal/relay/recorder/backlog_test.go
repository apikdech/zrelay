package recorder_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/clip"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/recorder"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
)

// TestRunAppliesBacklogPolicy: the egress backlog policy runs on other
// goroutines, so the loop applies it on the recorder goroutine. A force signal
// ends the clip with backlog_overflow and suppresses the camera (relay-spec
// §4.5); the camera leaves Suppressed once the resume check allows it.
func TestRunAppliesBacklogPolicy(t *testing.T) {
	fc := clock.NewFake(t0)
	hub := camera.NewHub(cameraID)
	sub := hub.Subscribe(camera.ClassRecorder, 4096)
	tracker := events.NewTracker(time.Minute, fc)
	out := &fakeOutput{}
	m := clip.New(clip.Config{Camera: cameraID, PostRoll: 5 * time.Second}, fc, ring.New(time.Second, 64<<20, nil), out)

	force := make(chan struct{}, 1)
	var resume atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		recorder.Run(ctx, sub, tracker, m, recorder.WithBacklogPolicy(force, resume.Load))
		close(done)
	}()
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

	force <- struct{}{}
	eventually(t, "state Suppressed", func() bool { return m.State() == "Suppressed" })
	if f := w.finalInfo(); f == nil || f.Reason != clip.ReasonBacklogOverflow {
		t.Fatalf("final = %+v, want backlog_overflow", f)
	}

	// While resume says no, the camera stays suppressed across ticks.
	time.Sleep(600 * time.Millisecond)
	if s := m.State(); s != "Suppressed" {
		t.Fatalf("state = %s before resume, want Suppressed", s)
	}
	resume.Store(true)
	eventually(t, "left Suppressed", func() bool { return m.State() != "Suppressed" })
}
