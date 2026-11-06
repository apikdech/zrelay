package camera_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

var superviseStart = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// stateLog records the Status values passed to OnState.
type stateLog struct {
	mu   sync.Mutex
	list []camera.Status
}

func (l *stateLog) add(s camera.Status) {
	l.mu.Lock()
	l.list = append(l.list, s)
	l.mu.Unlock()
}

func (l *stateLog) states() []camera.State {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]camera.State, len(l.list))
	for i, s := range l.list {
		out[i] = s.State
	}
	return out
}

// inOrder reports whether want is a subsequence of got.
func inOrder(got []camera.State, want ...camera.State) bool {
	i := 0
	for _, s := range got {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}

func newPullPoint(id string, f *testutil.FakeONVIF) *events.PullPoint {
	return &events.PullPoint{Camera: id, Host: f.Host, Port: f.Port, Username: "admin", Password: "secret", Rules: events.TapoRules()}
}

// eventually polls cond every few milliseconds, advancing clk by a second
// each time when clk is not nil, until it holds or 20s pass.
func eventually(t *testing.T, clk *clock.Fake, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		if clk != nil {
			clk.Advance(time.Second)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func superviseCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func loopingRTSP(t *testing.T) *testutil.RTSPServer {
	t.Helper()
	return testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: testutil.LoadAnnexB(t, h264Fixture), Loop: true})
}

func TestSupervisorStates(t *testing.T) {
	rtsp := loopingRTSP(t)
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	clk := clock.NewFake(superviseStart)
	var log stateLog
	cam := camera.Supervise(superviseCtx(t), camera.SuperviseConfig{
		ID: "cam1", RTSPURL: rtsp.URL(), Events: newPullPoint("cam1", onvif),
		TriggerTimeout: time.Minute, Clock: clk, OnState: log.add,
	})
	if cam.ID != "cam1" || cam.Hub == nil || cam.Tracker == nil || cam.Status == nil {
		t.Fatalf("Camera = %+v, want ID, Hub, Tracker and Status set", cam)
	}
	if got := log.states(); len(got) == 0 || got[0] != camera.StateConnecting {
		t.Fatalf("first reported state = %v, want connecting", got)
	}

	// The fake clock stands still, so only real progress makes it ready.
	eventually(t, nil, "ready", func() bool { return cam.Status().State == camera.StateReady })
	st := cam.Status()
	if st.RTSP != "up" || st.Events != "up" || !st.Since.Equal(superviseStart) {
		t.Errorf("ready status = %+v, want rtsp up, events up, since %v", st, superviseStart)
	}
	if got := log.states(); !inOrder(got, camera.StateConnecting, camera.StatePriming, camera.StateReady) {
		t.Errorf("states = %v, want connecting, priming, ready in order", got)
	}

	// Losing the RTSP session degrades the camera until the backoff passes.
	rtsp.Disconnect()
	eventually(t, nil, "degraded", func() bool { return cam.Status().State == camera.StateDegraded })
	if st := cam.Status(); st.RTSP != "down" {
		t.Errorf("degraded status = %+v, want rtsp down", st)
	}
	eventually(t, clk, "ready again", func() bool { return cam.Status().State == camera.StateReady })
	if got := log.states(); !inOrder(got, camera.StateConnecting, camera.StatePriming, camera.StateReady,
		camera.StateDegraded, camera.StateConnecting, camera.StatePriming, camera.StateReady) {
		t.Errorf("states = %v, want a full cycle after Disconnect", got)
	}
}

// TestEventsDownKeepsVideoAndMarksDegraded: ONVIF unreachable while RTSP
// works (Review Focus 5).
func TestEventsDownKeepsVideoAndMarksDegraded(t *testing.T) {
	rtsp := loopingRTSP(t)
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	for range 100 {
		onvif.Fault("CreatePullPointSubscription")
	}
	cam := camera.Supervise(superviseCtx(t), camera.SuperviseConfig{
		ID: "cam1", RTSPURL: rtsp.URL(), Events: newPullPoint("cam1", onvif),
		TriggerTimeout: time.Minute, Clock: clock.NewFake(superviseStart),
	})
	sub := cam.Hub.Subscribe(camera.ClassViewer, 64)
	defer sub.Close()

	eventually(t, nil, "degraded", func() bool { return cam.Status().State == camera.StateDegraded })
	if st := cam.Status(); st.Events != "down" || st.RTSP != "up" {
		t.Fatalf("status = %+v, want events down and rtsp up", st)
	}

	// Video still reaches subscribers.
	videos := 0
	deadline := time.After(20 * time.Second)
	for videos < 10 {
		select {
		case ev := <-sub.C():
			if ev.Kind == camera.EventVideo {
				videos++
			}
		case <-deadline:
			t.Fatalf("got %d video events while events are down, want 10", videos)
		}
	}
	if st := cam.Status(); st.State != camera.StateDegraded {
		t.Errorf("state = %v after video flowed, want degraded", st.State)
	}
	if cam.Tracker.Triggered() {
		t.Error("tracker is triggered although events are down")
	}
}

func TestOneCameraFailureIsolated(t *testing.T) {
	rtsp := loopingRTSP(t)
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "rtsp://" + l.Addr().String() + "/stream"
	l.Close() // nothing listens here any more

	ctx := superviseCtx(t)
	dead := camera.Supervise(ctx, camera.SuperviseConfig{
		ID: "dead", RTSPURL: deadURL, Events: newPullPoint("dead", onvif),
		TriggerTimeout: time.Minute, Clock: clock.NewFake(superviseStart),
	})
	good := camera.Supervise(ctx, camera.SuperviseConfig{
		ID: "good", RTSPURL: rtsp.URL(), Events: newPullPoint("good", onvif),
		TriggerTimeout: time.Minute, Clock: clock.NewFake(superviseStart),
	})
	eventually(t, nil, "the good camera to be ready", func() bool { return good.Status().State == camera.StateReady })
	eventually(t, nil, "the dead camera to fail", func() bool { return dead.Status().State == camera.StateDegraded })
	if st := dead.Status(); st.RTSP != "down" {
		t.Errorf("dead camera status = %+v, want rtsp down", st)
	}
	if st := good.Status(); st.State != camera.StateReady {
		t.Errorf("good camera state = %v after the other failed, want ready", st.State)
	}
}

// TestTriggerTimeoutExpires checks that the supervisor expires a kind whose
// inactive message never arrives, using the camera's TriggerTimeout.
func TestTriggerTimeoutExpires(t *testing.T) {
	rtsp := loopingRTSP(t)
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	clk := clock.NewFake(superviseStart)
	cam := camera.Supervise(superviseCtx(t), camera.SuperviseConfig{
		ID: "cam1", RTSPURL: rtsp.URL(), Events: newPullPoint("cam1", onvif),
		TriggerTimeout: time.Minute, Clock: clk,
	})
	onvif.Queue("tns1:RuleEngine/CellMotionDetector/Motion", "Changed", events.Item{Name: "IsMotion", Value: "true"})
	eventually(t, nil, "motion", func() bool { return cam.Tracker.Triggered() })
	clk.Advance(2 * time.Minute)
	eventually(t, clk, "motion to expire", func() bool { return !cam.Tracker.Triggered() })
}
