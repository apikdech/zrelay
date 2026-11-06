package restream_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/restream"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const (
	fixture = "../../../testdata/video/h264-640x360-gop2-10s.h264"
	user    = "viewer"
	pass    = "s3cret-pass"
	camName = "tapo-c220"
)

type badStatus = liberrors.ErrClientBadStatusCode

// rig is a fake camera feeding a Hub, and a restream Server in front of it.
type rig struct {
	hub *camera.Hub
	srv *restream.Server
}

// startRig runs a looping test RTSP camera into a Hub and starts the restream
// server on an ephemeral port.
func startRig(t *testing.T, audio bool) *rig {
	t.Helper()
	aus := testutil.LoadAnnexB(t, fixture)
	cam := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, Loop: true, Audio: audio})
	hub := camera.NewHub(camName)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = camera.RunRTSP(ctx, cam.URL(), hub)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	srv, err := restream.New(restream.Config{
		Listen:      "127.0.0.1:0",
		Username:    user,
		Password:    pass,
		Hubs:        map[string]*camera.Hub{camName: hub},
		ViewerQueue: 256,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Close)
	return &rig{hub: hub, srv: srv}
}

func (r *rig) url(creds string) string {
	return fmt.Sprintf("rtsp://%s%s/%s", creds, r.srv.Addr(), camName)
}

func (r *rig) goodURL() string { return r.url(user + ":" + pass + "@") }

// newClient returns a started client for rawURL. proto nil means TCP.
func newClient(t *testing.T, rawURL string, proto *gortsplib.Protocol) (*gortsplib.Client, *base.URL) {
	t.Helper()
	u, err := base.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if proto == nil {
		tcp := gortsplib.ProtocolTCP
		proto = &tcp
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: proto}
	if err := c.Start(); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	t.Cleanup(c.Close)
	return c, u
}

// connect starts a client and describes the stream, retrying while the
// camera has not published yet (404).
func connect(t *testing.T, rawURL string, proto *gortsplib.Protocol) (*gortsplib.Client, *description.Session) {
	t.Helper()
	c, u := newClient(t, rawURL, proto)
	deadline := time.Now().Add(15 * time.Second)
	for {
		desc, _, err := c.Describe(u)
		if err == nil {
			return c, desc
		}
		var bad badStatus
		if errors.As(err, &bad) && bad.Code == base.StatusNotFound && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		t.Fatalf("Describe: %v", err)
	}
}

func TestRestreamServesVideo(t *testing.T) {
	t.Parallel()
	r := startRig(t, false)
	c, desc := connect(t, r.goodURL(), nil)
	if len(desc.Medias) != 1 || desc.Medias[0].Type != description.MediaTypeVideo {
		t.Fatalf("medias = %+v, want one video track", desc.Medias)
	}
	h264, ok := desc.Medias[0].Formats[0].(*format.H264)
	if !ok || len(h264.SPS) == 0 || len(h264.PPS) == 0 {
		t.Fatalf("format = %#v, want H264 with SPS and PPS", desc.Medias[0].Formats[0])
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var n atomic.Int32
	got := make(chan struct{})
	c.OnPacketRTPAny(func(_ *description.Media, _ format.Format, _ *rtp.Packet) {
		if n.Add(1) == 30 {
			close(got)
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("Play: %v", err)
	}
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatalf("only %d RTP packets", n.Load())
	}
	if n := r.srv.Clients(camName); n != 1 {
		t.Errorf("Clients = %d, want 1", n)
	}
	if n := r.srv.Clients("nope"); n != 0 {
		t.Errorf("Clients(unknown) = %d, want 0", n)
	}
}

// rawRTSP sends one request over a plain TCP connection and returns the
// status line.
func rawRTSP(t *testing.T, addr, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return strings.TrimSpace(line)
}

func TestRestreamAuthRequired(t *testing.T) {
	t.Parallel()
	r := startRig(t, false)
	target := fmt.Sprintf("rtsp://%s/%s", r.srv.Addr(), camName)

	noAuth := fmt.Sprintf("DESCRIBE %s RTSP/1.0\r\nCSeq: 1\r\n\r\n", target)
	if got := rawRTSP(t, r.srv.Addr(), noAuth); !strings.Contains(got, " 401") {
		t.Errorf("no credentials: %q, want 401", got)
	}
	wrong := fmt.Sprintf("DESCRIBE %s RTSP/1.0\r\nCSeq: 1\r\nAuthorization: Basic %s\r\n\r\n",
		target, "dmlld2VyOndyb25n") // viewer:wrong
	if got := rawRTSP(t, r.srv.Addr(), wrong); !strings.Contains(got, " 401") {
		t.Errorf("wrong credentials: %q, want 401", got)
	}

	// A real client with the wrong password never gets a description.
	c, u := newClient(t, r.url("viewer:wrong@"), nil)
	if _, _, err := c.Describe(u); err == nil {
		t.Error("Describe with wrong password succeeded")
	}
}

func TestRestreamRejectsTunnels(t *testing.T) {
	t.Parallel()
	r := startRig(t, false)
	for name, tun := range map[string]gortsplib.Tunnel{"http": gortsplib.TunnelHTTP, "websocket": gortsplib.TunnelWebSocket} {
		t.Run(name, func(t *testing.T) {
			u, err := base.ParseURL(r.goodURL())
			if err != nil {
				t.Fatal(err)
			}
			tcp := gortsplib.ProtocolTCP
			c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Tunnel: tun, Protocol: &tcp}
			if err := c.Start(); err != nil {
				return // refused while opening the tunnel: fine
			}
			defer c.Close()
			_, _, err = c.Describe(u)
			if err == nil {
				t.Fatal("Describe over a tunnel succeeded")
			}
			var bad badStatus
			if errors.As(err, &bad) && bad.Code != base.StatusForbidden {
				t.Errorf("status = %d, want 403", bad.Code)
			}
		})
	}
	// A bare HTTP request (what a web page could send) gets no stream.
	resp, err := http.Get("http://" + r.srv.Addr() + "/" + camName)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("plain HTTP GET returned 200")
		}
	}
}

func TestRestreamUDPRefused(t *testing.T) {
	t.Parallel()
	r := startRig(t, false)
	udp := gortsplib.ProtocolUDP
	c, desc := connect(t, r.goodURL(), &udp)
	_, err := c.Setup(desc.BaseURL, desc.Medias[0], 0, 0)
	if err == nil {
		t.Fatal("UDP SETUP succeeded")
	}
	var bad badStatus
	if !errors.As(err, &bad) || bad.Code != base.StatusUnsupportedTransport {
		t.Errorf("SETUP error = %v, want status 461", err)
	}
}

func TestRestreamClientsDroppedOnCameraLost(t *testing.T) {
	t.Parallel()
	r := startRig(t, false)
	var codec camera.Codec
	var ts camera.TimeSource
	waitFor(t, "camera codec", func() bool {
		var ok bool
		codec, ts, ok = r.hub.CurrentCodec()
		return ok
	})

	c, desc := connect(t, r.goodURL(), nil)
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Play(nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one client", func() bool { return r.srv.Clients(camName) == 1 })

	r.hub.Lost()

	waited := make(chan error, 1)
	go func() { waited <- c.Wait() }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("client session still alive after Hub.Lost")
	}
	waitFor(t, "zero clients", func() bool { return r.srv.Clients(camName) == 0 })

	// Until the camera is back there is nothing to describe.
	c2, u := newClient(t, r.goodURL(), nil)
	_, _, err := c2.Describe(u)
	var bad badStatus
	if !errors.As(err, &bad) || bad.Code != base.StatusNotFound {
		t.Errorf("Describe while camera lost: %v, want 404", err)
	}

	// The next session (Codec call) is served again.
	r.hub.Codec(codec, ts)
	connect(t, r.goodURL(), nil)
}

func TestRestreamAudioTrackWhenPresent(t *testing.T) {
	t.Parallel()
	r := startRig(t, true)
	c, u := newClient(t, r.goodURL(), nil)

	// The audio track appears once the camera's first audio packet arrives.
	var desc *description.Session
	waitFor(t, "audio track in description", func() bool {
		d, _, err := c.Describe(u)
		if err != nil || len(d.Medias) != 2 {
			return false
		}
		desc = d
		return true
	})
	if desc.Medias[0].Type != description.MediaTypeVideo || desc.Medias[1].Type != description.MediaTypeAudio {
		t.Fatalf("medias = %v/%v, want video then audio", desc.Medias[0].Type, desc.Medias[1].Type)
	}
	g711, ok := desc.Medias[1].Formats[0].(*format.G711)
	if !ok || g711.MULaw || g711.SampleRate != 8000 {
		t.Fatalf("audio format = %#v, want PCMA 8000", desc.Medias[1].Formats[0])
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatal(err)
	}
	audio := make(chan *rtp.Packet, 1)
	c.OnPacketRTP(desc.Medias[1], g711, func(p *rtp.Packet) {
		select {
		case audio <- p:
		default:
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-audio:
		if p.PayloadType != 8 || len(p.Payload) == 0 {
			t.Errorf("audio packet pt=%d len=%d", p.PayloadType, len(p.Payload))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no audio RTP packet")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	t.Parallel()
	hubs := map[string]*camera.Hub{"a": camera.NewHub("a")}
	for name, cfg := range map[string]restream.Config{
		"no listen":   {Username: "u", Password: "p", Hubs: hubs},
		"no username": {Listen: "127.0.0.1:0", Password: "p", Hubs: hubs},
		"no password": {Listen: "127.0.0.1:0", Username: "u", Hubs: hubs},
		"nil hub":     {Listen: "127.0.0.1:0", Username: "u", Password: "p", Hubs: map[string]*camera.Hub{"a": nil}},
		"bad name":    {Listen: "127.0.0.1:0", Username: "u", Password: "p", Hubs: map[string]*camera.Hub{"a/b": camera.NewHub("a/b")}},
	} {
		if _, err := restream.New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
