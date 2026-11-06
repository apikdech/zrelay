package live

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/apikdech/zrelay-v2/internal/bearer"
	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const h264Fixture = "../../../testdata/video/h264-640x360-gop2-10s.h264"

// testToken returns a verifier and the matching bearer credential.
func testToken(t testing.TB) (*bearer.Verifier, string) {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i*7 + 3)
	}
	sum := sha256.Sum256(raw)
	v, err := bearer.New([]string{hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	return v, base64.RawURLEncoding.EncodeToString(raw)
}

// naluType returns the H.264 NAL unit type of n.
func naluType(n []byte) byte { return n[0] & 0x1f }

// fixtureCodec extracts the SPS and PPS of the first access unit.
func fixtureCodec(aus []testutil.AU) camera.Codec {
	c := camera.Codec{Kind: "h264", Width: 640, Height: 360}
	for _, n := range aus[0].NALUs {
		switch naluType(n) {
		case 7:
			c.SPS = n
		case 8:
			c.PPS = n
		}
	}
	return c
}

// splitAnnexB splits an Annex-B byte stream into NAL units.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, trimZeros(b[start:i]))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, trimZeros(b[start:]))
	}
	return out
}

func trimZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// publisher feeds a Hub with the fixture's access units in a loop, with
// monotonically increasing DTS at 15 fps, paced every interval.
type publisher struct {
	stop context.CancelFunc
	wg   sync.WaitGroup
}

func startPublisher(t testing.TB, hub *camera.Hub, interval time.Duration, audio bool) *publisher {
	t.Helper()
	aus := testutil.LoadAnnexB(t, h264Fixture)
	hub.Codec(fixtureCodec(aus), camera.TimeSourceArrival)
	ctx, cancel := context.WithCancel(context.Background())
	p := &publisher{stop: cancel}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		var dts int64
		var seq uint16
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			au := aus[i%len(aus)]
			n := 0
			for _, nal := range au.NALUs {
				n += len(nal)
			}
			hub.Video(&camera.AccessUnit{PTS: dts, DTS: dts, IDR: au.IDR, NALUs: au.NALUs, Wall: time.Now().UTC(), Bytes: n})
			dts += 6000
			if audio {
				seq++
				hub.Audio(&camera.AudioPacket{Codec: "PCMA", Payload: bytes.Repeat([]byte{0xd5}, 160), Timestamp: uint32(seq) * 160, Seq: seq})
			}
		}
	}()
	t.Cleanup(p.Stop)
	return p
}

// Stop ends the publisher; it is safe to call more than once.
func (p *publisher) Stop() { p.stop(); p.wg.Wait() }

type testServer struct {
	*Server
	http  *httptest.Server
	token string
}

func newTestServer(t testing.TB, hubs map[string]*camera.Hub, maxViewers int) *testServer {
	t.Helper()
	return newTestServerWith(t, hubs, maxViewers, nil)
}

// newTestServerWith is newTestServer with a hook to adjust the Config.
func newTestServerWith(t testing.TB, hubs map[string]*camera.Hub, maxViewers int, adjust func(*Config)) *testServer {
	t.Helper()
	ver, token := testToken(t)
	cfg := Config{
		Listen:      "127.0.0.1:0",
		AdvertiseIP: "127.0.0.1",
		MaxViewers:  maxViewers,
		Verifier:    ver,
		Hubs:        hubs,
		ViewerQueue: 64,
		Clock:       clock.Real,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { h.Close(); s.Close() })
	return &testServer{Server: s, http: h, token: token}
}

// offerClient is an in-process pion WHEP client on loopback.
type offerClient struct {
	pc    *webrtc.PeerConnection
	video chan []byte // depacketized Annex-B per RTP packet
	audio chan string // codec mime of audio tracks seen
	loc   string
}

func newOfferClient(t testing.TB, wantAudio bool) (*offerClient, string) {
	t.Helper()
	return newOfferClientOn(t, wantAudio, nil)
}

// newOfferClientOn is newOfferClient with all ICE traffic on conn, so a test
// can silence the client by closing conn without closing the peer connection.
func newOfferClientOn(t testing.TB, wantAudio bool, conn net.PacketConn) (*offerClient, string) {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if conn != nil {
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, conn))
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	c := &offerClient{pc: pc, video: make(chan []byte, 4096), audio: make(chan string, 16)}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if wantAudio {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if tr.Kind() == webrtc.RTPCodecTypeAudio {
			select {
			case c.audio <- tr.Codec().MimeType:
			default:
			}
		}
		var depay codecs.H264Packet
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			if tr.Kind() != webrtc.RTPCodecTypeVideo {
				continue
			}
			out, err := depay.Unmarshal(pkt.Payload)
			if err != nil || len(out) == 0 {
				continue
			}
			select {
			case c.video <- out:
			default:
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	return c, pc.LocalDescription().SDP
}

// post sends an offer to the server and returns the response.
func (ts *testServer) post(t testing.TB, camID, contentType, auth string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.http.URL+"/live/"+camID, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// connect negotiates c with the server and applies the answer.
func (ts *testServer) connect(t testing.TB, c *offerClient, offer, camID string) {
	t.Helper()
	resp := ts.post(t, camID, "application/sdp", "Bearer "+ts.token, bytes.NewReader([]byte(offer)))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/sdp" {
		t.Fatalf("answer Content-Type %q", ct)
	}
	c.loc = resp.Header.Get("Location")
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
}

func eventually(t testing.TB, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
