package testutil_test

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const waitLimit = 3 * time.Second

// dialAndSetup connects a TCP gortsplib client, describes and sets up all
// medias. The caller registers callbacks and then calls Play.
func dialAndSetup(t *testing.T, rawURL string) (*gortsplib.Client, *description.Session) {
	t.Helper()
	u, err := base.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	tcp := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tcp}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatal(err)
	}
	return c, desc
}

func play(t *testing.T, c *gortsplib.Client) {
	t.Helper()
	if _, err := c.Play(nil); err != nil {
		t.Fatal(err)
	}
}

func h264Medi(t *testing.T, desc *description.Session) (*description.Media, *format.H264) {
	t.Helper()
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		t.Fatal("no H264 media in SDP")
	}
	return medi, forma
}

// packetSink collects RTP packets from a client callback.
type packetSink struct {
	mu   sync.Mutex
	pkts []*rtp.Packet
	ch   chan struct{}
}

func newSink() *packetSink { return &packetSink{ch: make(chan struct{}, 1)} }

func (s *packetSink) add(p *rtp.Packet) {
	s.mu.Lock()
	s.pkts = append(s.pkts, p.Clone())
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

func (s *packetSink) snapshot() []*rtp.Packet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rtp.Packet(nil), s.pkts...)
}

// waitFor blocks until cond holds on the collected packets or the limit passes.
func (s *packetSink) waitFor(t *testing.T, what string, cond func([]*rtp.Packet) bool) []*rtp.Packet {
	t.Helper()
	deadline := time.NewTimer(waitLimit)
	defer deadline.Stop()
	for {
		if p := s.snapshot(); cond(p) {
			return p
		}
		select {
		case <-s.ch:
		case <-deadline.C:
			t.Fatalf("timed out after %v waiting for %s (got %d packets)", waitLimit, what, len(s.snapshot()))
		}
	}
}

func atLeast(n int) func([]*rtp.Packet) bool {
	return func(p []*rtp.Packet) bool { return len(p) >= n }
}

func TestRTSPServerServesFixture(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, Loop: true})

	c, desc := dialAndSetup(t, srv.URL())
	medi, forma := h264Medi(t, desc)
	if len(forma.SPS) == 0 || len(forma.PPS) == 0 {
		t.Fatal("SDP lacks sprop-parameter-sets")
	}
	sink := newSink()
	c.OnPacketRTP(medi, forma, func(p *rtp.Packet) { sink.add(p) })
	play(t, c)

	sink.waitFor(t, ">= 30 RTP packets", atLeast(30))
}

func TestRTSPServerStartTimestampHonoured(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	const start = uint32(0xFFFFA000) // wraps within the first AUs
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{
		Codec: "h264", AUs: aus, FPS: 15, Loop: true, StartRTPTimestamp: start,
	})

	c, desc := dialAndSetup(t, srv.URL())
	medi, forma := h264Medi(t, desc)
	sink := newSink()
	c.OnPacketRTP(medi, forma, func(p *rtp.Packet) { sink.add(p) })
	play(t, c)

	distinctTS := func(p []*rtp.Packet) []uint32 {
		var ts []uint32
		for _, pk := range p {
			if len(ts) == 0 || ts[len(ts)-1] != pk.Timestamp {
				ts = append(ts, pk.Timestamp)
			}
		}
		return ts
	}
	pkts := sink.waitFor(t, "5 access units", func(p []*rtp.Packet) bool { return len(distinctTS(p)) >= 5 })
	ts := distinctTS(pkts)
	if ts[0] != start {
		t.Fatalf("first RTP timestamp = %#x, want %#x", ts[0], start)
	}
	for i := 1; i < len(ts); i++ {
		if want := start + uint32(i)*6000; ts[i] != want {
			t.Fatalf("AU %d RTP timestamp = %#x, want %#x", i, ts[i], want)
		}
	}
}

func TestRTSPServerServesH265(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h265Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h265", AUs: aus, FPS: 15, Loop: true})

	c, desc := dialAndSetup(t, srv.URL())
	var forma *format.H265
	medi := desc.FindFormat(&forma)
	if medi == nil {
		t.Fatal("no H265 media in SDP")
	}
	if len(forma.VPS) == 0 || len(forma.SPS) == 0 || len(forma.PPS) == 0 {
		t.Fatal("SDP lacks sprop-vps/sps/pps")
	}
	sink := newSink()
	c.OnPacketRTP(medi, forma, func(p *rtp.Packet) { sink.add(p) })
	play(t, c)
	sink.waitFor(t, ">= 30 RTP packets", atLeast(30))
}

func TestRTSPServerAudioPCMA(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, Loop: true, Audio: true})

	c, desc := dialAndSetup(t, srv.URL())
	var g711 *format.G711
	medi := desc.FindFormat(&g711)
	if medi == nil {
		t.Fatal("no G711 media in SDP")
	}
	if g711.MULaw || g711.SampleRate != 8000 || g711.ChannelCount != 1 {
		t.Fatalf("audio format = %+v, want PCMA 8 kHz mono", g711)
	}
	sink := newSink()
	c.OnPacketRTP(medi, g711, func(p *rtp.Packet) { sink.add(p) })
	play(t, c)
	pkts := sink.waitFor(t, ">= 10 audio packets", atLeast(10))
	for i := 1; i < len(pkts); i++ {
		if d := pkts[i].Timestamp - pkts[i-1].Timestamp; d != uint32(len(pkts[i-1].Payload)) {
			t.Fatalf("audio timestamp step %d != previous payload %d samples", d, len(pkts[i-1].Payload))
		}
	}
}

func TestRTSPServerNoAudioByDefault(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15})
	_, desc := dialAndSetup(t, srv.URL())
	if len(desc.Medias) != 1 {
		t.Fatalf("got %d medias, want 1 (video only)", len(desc.Medias))
	}
}

func TestRTSPServerRTCPSenderReports(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	const start = uint32(123456)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{
		Codec: "h264", AUs: aus, FPS: 15, Loop: true, RTCP: true, StartRTPTimestamp: start,
	})

	c, desc := dialAndSetup(t, srv.URL())
	medi, forma := h264Medi(t, desc)
	srs := make(chan *rtcp.SenderReport, 16)
	c.OnPacketRTP(medi, forma, func(*rtp.Packet) {})
	c.OnPacketRTCP(medi, func(p rtcp.Packet) {
		if sr, ok := p.(*rtcp.SenderReport); ok {
			select {
			case srs <- sr:
			default:
			}
		}
	})
	play(t, c)

	select {
	case sr := <-srs:
		if sr.RTPTime-start > 10*90000 {
			t.Fatalf("sender report RTP time %d not near start %d", sr.RTPTime, start)
		}
	case <-time.After(waitLimit):
		t.Fatal("no RTCP sender report received")
	}
}

func TestRTSPServerDisconnectDropsSessions(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, Loop: true})

	c, desc := dialAndSetup(t, srv.URL())
	medi, forma := h264Medi(t, desc)
	sink := newSink()
	c.OnPacketRTP(medi, forma, func(p *rtp.Packet) { sink.add(p) })
	play(t, c)
	sink.waitFor(t, "first packets", atLeast(1))

	srv.Disconnect()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("client Wait returned nil after Disconnect")
		}
	case <-time.After(waitLimit):
		t.Fatal("client still connected after Disconnect")
	}

	// The server keeps accepting new sessions.
	c2, desc2 := dialAndSetup(t, srv.URL())
	medi2, forma2 := h264Medi(t, desc2)
	sink2 := newSink()
	c2.OnPacketRTP(medi2, forma2, func(p *rtp.Packet) { sink2.add(p) })
	play(t, c2)
	sink2.waitFor(t, "packets after reconnect", atLeast(1))
}

func TestRTSPServerSetParamSets(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	srv := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{Codec: "h264", AUs: aus, FPS: 15, Loop: true})

	sps := []byte{0x67, 0x64, 0x00, 0x1e, 0xac, 0xd9, 0x40, 0xa0}
	pps := []byte{0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0}
	srv.SetParamSets(sps, pps)

	c, desc := dialAndSetup(t, srv.URL())
	medi, forma := h264Medi(t, desc)
	if !bytes.Equal(forma.SPS, sps) || !bytes.Equal(forma.PPS, pps) {
		t.Fatalf("SDP parameter sets not updated: sps=%x pps=%x", forma.SPS, forma.PPS)
	}

	dec, err := forma.CreateDecoder()
	if err != nil {
		t.Fatal(err)
	}
	gotSPS, gotPPS := make(chan []byte, 64), make(chan []byte, 64)
	c.OnPacketRTP(medi, forma, func(p *rtp.Packet) {
		au, err := dec.Decode(p)
		if err != nil {
			if err != rtph264.ErrMorePacketsNeeded && err != rtph264.ErrNonStartingPacketAndNoPrevious {
				t.Errorf("decode: %v", err)
			}
			return
		}
		for _, n := range au {
			switch n[0] & 0x1f {
			case 7:
				gotSPS <- append([]byte(nil), n...)
			case 8:
				gotPPS <- append([]byte(nil), n...)
			}
		}
	})
	play(t, c)

	for _, w := range []struct {
		name string
		ch   chan []byte
		want []byte
	}{{"SPS", gotSPS, sps}, {"PPS", gotPPS, pps}} {
		select {
		case got := <-w.ch:
			if !bytes.Equal(got, w.want) {
				t.Fatalf("in-band %s = %x, want %x", w.name, got, w.want)
			}
		case <-time.After(waitLimit):
			t.Fatalf("no in-band %s received", w.name)
		}
	}
}
