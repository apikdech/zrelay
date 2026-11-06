package live

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

func TestWHEPOfferAnswerDeliversVideo(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 4)
	startPublisher(t, hub, 15*time.Millisecond, false)

	c, offer := newOfferClient(t, false)
	ts.connect(t, c, offer, "cam1")
	if !strings.HasPrefix(c.loc, "/live/cam1/") {
		t.Fatalf("Location %q", c.loc)
	}
	if got := ts.Viewers("cam1"); got != 1 {
		t.Fatalf("Viewers = %d, want 1", got)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case annexB := <-c.video:
			// The first slice NAL the client sees must be an IDR (type 5).
			for _, n := range splitAnnexB(annexB) {
				switch naluType(n) {
				case 5:
					return
				case 1:
					t.Fatalf("first decoded slice is a non-IDR (type 1)")
				}
			}
		case <-deadline:
			t.Fatal("no H.264 RTP within 3s")
		}
	}
}

func TestWHEPAudioNegotiatedWhenPresent(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 4)
	startPublisher(t, hub, 15*time.Millisecond, true)
	eventually(t, 3*time.Second, "audio seen by server", func() bool { return ts.audioCodec("cam1") == "PCMA" })

	c, offer := newOfferClient(t, true)
	ts.connect(t, c, offer, "cam1")
	select {
	case mime := <-c.audio:
		if !strings.EqualFold(mime, "audio/PCMA") {
			t.Fatalf("audio track %q, want audio/PCMA", mime)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no PCMA audio track")
	}
}

func TestWHEPNoAudioTrackWhenCameraHasNone(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 4)
	startPublisher(t, hub, 15*time.Millisecond, false)

	c, offer := newOfferClient(t, true)
	ts.connect(t, c, offer, "cam1")
	select {
	case <-c.video:
	case <-time.After(5 * time.Second):
		t.Fatal("no video")
	}
	select {
	case m := <-c.audio:
		t.Fatalf("unexpected audio track %q", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWHEPAuth(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 4)
	_, offer := newOfferClient(t, false)
	for name, auth := range map[string]string{
		"missing": "",
		"wrong":   "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"scheme":  "Basic " + ts.token,
		"garbage": "Bearer !!!",
	} {
		resp := ts.post(t, "cam1", "application/sdp", auth, strings.NewReader(offer))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	// DELETE needs auth too.
	req, _ := http.NewRequest(http.MethodDelete, ts.http.URL+"/live/cam1/abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("DELETE without auth: %d, want 401", resp.StatusCode)
	}
	if n := ts.created.Load(); n != 0 {
		t.Errorf("%d pion sessions created for rejected requests", n)
	}
	if ts.Viewers("cam1") != 0 {
		t.Error("viewer counted for rejected request")
	}
}

func TestWHEPMaxViewers(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 1)
	startPublisher(t, hub, 15*time.Millisecond, false)

	c1, offer1 := newOfferClient(t, false)
	ts.connect(t, c1, offer1, "cam1")
	_, offer2 := newOfferClient(t, false)
	resp := ts.post(t, "cam1", "application/sdp", "Bearer "+ts.token, strings.NewReader(offer2))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second POST status %d, want 503", resp.StatusCode)
	}
	if ts.Viewers("cam1") != 1 {
		t.Fatalf("Viewers = %d, want 1", ts.Viewers("cam1"))
	}
}

func TestWHEPDeleteEndsSession(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 1)
	startPublisher(t, hub, 15*time.Millisecond, false)

	c, offer := newOfferClient(t, false)
	ts.connect(t, c, offer, "cam1")
	if ts.Viewers("cam1") != 1 {
		t.Fatal("session not counted")
	}
	del := func(path, auth string) int {
		req, _ := http.NewRequest(http.MethodDelete, ts.http.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := del(c.loc, "Bearer "+ts.token); got != http.StatusNoContent {
		t.Fatalf("DELETE status %d, want 204", got)
	}
	if ts.Viewers("cam1") != 0 {
		t.Fatalf("Viewers = %d after DELETE", ts.Viewers("cam1"))
	}
	if got := del(c.loc, "Bearer "+ts.token); got != http.StatusNotFound {
		t.Fatalf("second DELETE status %d, want 404", got)
	}
	// The slot is free again.
	c2, offer2 := newOfferClient(t, false)
	ts.connect(t, c2, offer2, "cam1")
}

func TestWHEPUnknownCamera404(t *testing.T) {
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": camera.NewHub("cam1")}, 4)
	resp := ts.post(t, "nope", "application/sdp", "Bearer "+ts.token, strings.NewReader("v=0"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

func TestWHEPWrongContentType415(t *testing.T) {
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": camera.NewHub("cam1")}, 4)
	resp := ts.post(t, "cam1", "application/json", "Bearer "+ts.token, strings.NewReader("{}"))
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", resp.StatusCode)
	}
}

func TestWHEPBodyLimit413(t *testing.T) {
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": camera.NewHub("cam1")}, 4)
	resp := ts.post(t, "cam1", "application/sdp", "Bearer "+ts.token, bytes.NewReader(bytes.Repeat([]byte("a"), 64<<10+1)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	if ts.created.Load() != 0 {
		t.Fatal("pion session created for oversized body")
	}
}

func TestWHEPBadOffer400(t *testing.T) {
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": camera.NewHub("cam1")}, 4)
	resp := ts.post(t, "cam1", "application/sdp", "Bearer "+ts.token, strings.NewReader("not sdp"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if ts.Viewers("cam1") != 0 {
		t.Fatal("failed negotiation still holds a viewer slot")
	}
}

func TestSessionClosedOnICEDisconnect(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 4)
	startPublisher(t, hub, 15*time.Millisecond, false)

	c, offer := newOfferClient(t, false)
	ts.connect(t, c, offer, "cam1")
	select {
	case <-c.video:
	case <-time.After(5 * time.Second):
		t.Fatal("no video")
	}
	c.pc.Close()
	eventually(t, 10*time.Second, "server session closed", func() bool { return ts.Viewers("cam1") == 0 })
}

func TestServerCloseEndsSessions(t *testing.T) {
	hub := camera.NewHub("cam1")
	ts := newTestServer(t, map[string]*camera.Hub{"cam1": hub}, 4)
	startPublisher(t, hub, 15*time.Millisecond, false)
	c, offer := newOfferClient(t, false)
	ts.connect(t, c, offer, "cam1")
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	if ts.Viewers("cam1") != 0 {
		t.Fatal("sessions remain after Close")
	}
}
