package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

const h264Fixture = "../../../../testdata/video/h264-640x360-gop2-10s.h264"

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

type info struct {
	Upstream string `json:"upstream"`
	Camera   string `json:"camera"`
	Key      string `json:"key"`
	Hash     string `json:"api_token_sha256"`
	Bearer   string `json:"bearer"`
}

func start(t *testing.T, extra ...string) (info, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan info, 1)
	out, errOut := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, append([]string{"--fixture", h264Fixture}, extra...), out, errOut, func(i info) { ready <- i })
	}()
	select {
	case i := <-ready:
		return i, func() int {
			cancel()
			select {
			case c := <-done:
				return c
			case <-time.After(15 * time.Second):
				t.Fatal("run did not return after cancel")
				return -1
			}
		}
	case c := <-done:
		t.Fatalf("run exited %d before ready; stderr:\n%s", c, errOut.String())
	case <-time.After(20 * time.Second):
		t.Fatal("not ready")
	}
	return info{}, nil
}

func offerSDP(t *testing.T) string {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
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
	return pc.LocalDescription().SDP
}

func whep(t *testing.T, i info, bearer, sdp string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, i.Upstream+"/live/"+i.Camera, strings.NewReader(sdp))
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

// TestLiveFixtureServesWHEP: the helper binds ephemeral ports, publishes the
// upstream URL and a fresh key, and the real live server answers an offer.
func TestLiveFixtureServesWHEP(t *testing.T) {
	i, stop := start(t, "--camera", "front", "--max-viewers", "1")
	if !strings.HasPrefix(i.Upstream, "http://127.0.0.1:") || strings.HasSuffix(i.Upstream, ":0") {
		t.Errorf("upstream %q is not an ephemeral loopback URL", i.Upstream)
	}
	if i.Camera != "front" || !strings.HasPrefix(i.Key, "zk1-") || len(i.Hash) != 64 || i.Bearer == "" {
		t.Errorf("unexpected info %+v", i)
	}
	sdp := offerSDP(t)
	if code, _ := whep(t, i, "", sdp); code != http.StatusUnauthorized {
		t.Errorf("no bearer: status %d, want 401", code)
	}
	code, answer := whep(t, i, i.Bearer, sdp)
	if code != http.StatusCreated || !strings.Contains(answer, "m=video") {
		t.Fatalf("offer: status %d, answer %q", code, answer)
	}
	// MaxViewers 1: the second viewer is refused.
	if code, _ := whep(t, i, i.Bearer, offerSDP(t)); code != http.StatusServiceUnavailable {
		t.Errorf("second viewer: status %d, want 503", code)
	}
	if c := stop(); c != 0 {
		t.Errorf("exit %d, want 0", c)
	}
}

// TestLiveFixtureInfoIsOneJSONLine: stdout carries exactly the info line.
func TestLiveFixtureInfoIsOneJSONLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errOut := &syncBuf{}, &syncBuf{}
	ready := make(chan struct{}, 1)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--fixture", h264Fixture}, out, errOut, func(info) { ready <- struct{}{} })
	}()
	select {
	case <-ready:
	case c := <-done:
		t.Fatalf("exit %d: %s", c, errOut.String())
	case <-time.After(20 * time.Second):
		t.Fatal("not ready")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines, want 1: %q", len(lines), out.String())
	}
	var i info
	if err := json.Unmarshal([]byte(lines[0]), &i); err != nil || i.Camera != "front" {
		t.Fatalf("stdout line %q: %v", lines[0], err)
	}
	cancel()
	<-done
}

func TestLiveFixtureUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if c := run(context.Background(), []string{"--nope"}, &out, &errOut, nil); c != 2 {
		t.Errorf("bad flag: exit %d, want 2", c)
	}
	if c := run(context.Background(), []string{"--fixture", "/does/not/exist.h264"}, &out, &errOut, nil); c != 2 {
		t.Errorf("missing fixture: exit %d, want 2", c)
	}
}
