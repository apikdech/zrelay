package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/zrec"
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

// vectorKey is the Contract §4.5 vector master key, which the fixture clips of
// testdata/clips/ are sealed with; the first-run e2e (Task 41) serves live video
// under the same key as the clips.
const vectorKey = "zk1-000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f-2418"

// TestLiveFixtureKeyRef: --key takes an ENV: ref (never a literal), and the
// printed key, token hash and bearer follow it.
func TestLiveFixtureKeyRef(t *testing.T) {
	t.Setenv("LIVEFIXTURE_TEST_KEY", vectorKey)
	i, stop := start(t, "--key", "ENV:LIVEFIXTURE_TEST_KEY")
	m, err := zrec.ParseMasterKey(vectorKey)
	if err != nil {
		t.Fatal(err)
	}
	keys := zrec.Derive(m)
	sum := zrec.APITokenHash(keys)
	if i.Key != vectorKey || i.Hash != hex.EncodeToString(sum[:]) || i.Bearer != zrec.BearerToken(keys) {
		t.Errorf("info %+v does not follow the key", i)
	}
	if code, _ := whep(t, i, i.Bearer, offerSDP(t)); code != http.StatusCreated {
		t.Errorf("offer under the given key: status %d, want 201", code)
	}
	if c := stop(); c != 0 {
		t.Errorf("exit %d, want 0", c)
	}
}

func TestLiveFixtureKeyRefRejected(t *testing.T) {
	var out, errOut bytes.Buffer
	for name, args := range map[string][]string{
		"literal":     {"--key", vectorKey},
		"unset ref":   {"--key", "ENV:LIVEFIXTURE_NOT_SET"},
		"file ref":    {"--key", "FILE:/nope"},
		"bad content": {"--key", "ENV:LIVEFIXTURE_BAD_KEY"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LIVEFIXTURE_BAD_KEY", "not-a-key")
			if c := run(context.Background(), append([]string{"--fixture", h264Fixture}, args...), &out, &errOut, nil); c != 2 {
				t.Errorf("exit %d, want 2", c)
			}
		})
	}
	if strings.Contains(errOut.String(), vectorKey) {
		t.Error("the key leaked into the error output")
	}
}
