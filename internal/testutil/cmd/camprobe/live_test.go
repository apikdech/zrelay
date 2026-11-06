package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/apikdech/zrelay-v2/internal/testutil"
)

// syncBuf is a bytes.Buffer that run's goroutines and the test can share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const (
	restreamUser = "viewer"
	restreamPass = "restream-pass-9f2"
)

// freeUDPPort returns a UDP port that was free a moment ago. The config
// validator rejects port 0 for live.ice_udp_port.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// liveRestreamConfig returns a probe config with live and restream enabled on
// ephemeral loopback ports, and the matching WHEP bearer token.
func liveRestreamConfig(t *testing.T, rtspURL string, onvifPort int) (cfg, token string) {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i*5 + 1)
	}
	sum := sha256.Sum256(raw)
	extra := fmt.Sprintf(`live:
  listen: "127.0.0.1:0"
  advertise_ip: "127.0.0.1"
  ice_udp_port: %d
  max_viewers: 2
  api_token_sha256: [%q]
restream:
  listen: "127.0.0.1:0"
  username: %q
  password: %q
`, freeUDPPort(t), hex.EncodeToString(sum[:]), restreamUser, restreamPass)
	return probeConfigWith(t, rtspURL, onvifPort, extra), base64.RawURLEncoding.EncodeToString(raw)
}

// waitAddr waits for a "<what> listening addr=<host:port>" line in out.
func waitAddr(t *testing.T, out *syncBuf, what string) string {
	t.Helper()
	re := regexp.MustCompile(what + ` listening addr=(\S+)`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			return m[1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q listening line in the output:\n%s", what, out.String())
	return ""
}

// TestCamprobeLiveAndRestream starts the probe with --live --restream against
// the test RTSP camera and fake ONVIF, then checks as a client that the WHEP
// endpoint answers an offer (and refuses a missing bearer) and that the
// restream serves RTP to a gortsplib client with credentials.
func TestCamprobeLiveAndRestream(t *testing.T) {
	rtsp := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: testutil.LoadAnnexB(t, h264Fixture), Loop: true})
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	cfg, token := liveRestreamConfig(t, rtsp.URL(), onvif.Port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errOut := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"--config", cfg, "--camera", "cam-1", "--interval", "500ms", "--live", "--restream",
		}, out, errOut)
	}()

	liveAddr := waitAddr(t, out, "live")
	restreamAddr := waitAddr(t, out, "restream")

	// WHEP: no bearer is refused, a bearer gets an answer with a video section.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	whep := func(bearer string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, "http://"+liveAddr+"/live/cam-1", strings.NewReader(pc.LocalDescription().SDP))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/sdp")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := whep(""); code != http.StatusUnauthorized {
		t.Errorf("WHEP without a bearer: status %d, want 401", code)
	}
	code, answer := whep(token)
	if code != http.StatusCreated {
		t.Fatalf("WHEP offer: status %d, want 201; body %q", code, answer)
	}
	if !strings.Contains(answer, "m=video") {
		t.Errorf("answer SDP has no video section:\n%s", answer)
	}

	// Restream: a TCP client with credentials reads RTP.
	u, err := base.ParseURL(fmt.Sprintf("rtsp://%s:%s@%s/cam-1", restreamUser, restreamPass, restreamAddr))
	if err != nil {
		t.Fatal(err)
	}
	tcp := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tcp}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var desc *description.Session
	for deadline := time.Now().Add(15 * time.Second); ; {
		desc, _, err = c.Describe(u)
		if err == nil {
			break
		}
		var bad liberrors.ErrClientBadStatusCode
		if errors.As(err, &bad) && bad.Code == base.StatusNotFound && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		t.Fatalf("Describe: %v", err)
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
		t.Fatalf("restream delivered only %d RTP packets", n.Load())
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d, want 0; stderr:\n%s", code, errOut.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	for _, secret := range []string{restreamPass, token, "secret"} {
		if strings.Contains(out.String()+errOut.String(), secret) {
			t.Errorf("the probe printed a secret (%.3s...)", secret)
		}
	}
}

// TestCamprobeLiveRestreamNeedConfig: --live and --restream are usage errors
// when the config leaves that server disabled.
func TestCamprobeLiveRestreamNeedConfig(t *testing.T) {
	cfg := probeConfig(t, "rtsp://127.0.0.1:1/stream", 1)
	for _, flag := range []string{"--live", "--restream"} {
		t.Run(flag, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(context.Background(), []string{"--config", cfg, "--camera", "cam-1", flag}, &out, &errOut)
			if code != 2 || !strings.Contains(errOut.String(), flag+" needs") {
				t.Fatalf("exit %d with stderr %q, want 2 naming the disabled section", code, errOut.String())
			}
		})
	}
}
