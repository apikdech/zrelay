package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"
)

// RTSPServerOpts configures StartRTSPServer.
type RTSPServerOpts struct {
	// Codec is "h264" (the default when empty) or "h265".
	Codec string
	// AUs are the access units to replay, in decode order.
	AUs []AU
	// FPS is the replay rate. It defaults to 15, the fixtures' rate.
	FPS int
	// Loop replays AUs forever. Otherwise the video stops after the last AU
	// (sessions stay open, as with a stalled camera).
	Loop bool
	// Audio adds a PCMA (G.711 A-law) 8 kHz mono track carrying silence.
	Audio bool
	// RTCP enables RTCP sender reports (NTP to RTP mapping). The first report
	// follows the first RTP packet, then one every 10 s.
	RTCP bool
	// StartRTPTimestamp is the RTP timestamp of the first access unit. Later
	// units add 90000/FPS each, wrapping at 2^32.
	StartRTPTimestamp uint32
}

// RTSPServer is an in-process RTSP server that replays synthetic access units
// like a camera. Replay starts at the first PLAY and then runs on its own
// clock: later readers join mid-stream, and timestamps continue across
// Disconnect.
type RTSPServer struct {
	t      testing.TB
	opts   RTSPServerOpts
	h265   bool
	server *gortsplib.Server
	stream *gortsplib.ServerStream
	video  *description.Media
	audio  *description.Media
	url    string

	mu          sync.Mutex
	conns       map[*gortsplib.ServerConn]struct{}
	sessions    map[*gortsplib.ServerSession]struct{}
	pendingPlay map[*gortsplib.ServerConn]struct{}
	sps, pps    []byte // SetParamSets override; nil when unset

	startOnce sync.Once
	started   chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
}

// StartRTSPServer starts a server on an ephemeral 127.0.0.1 port. It stops
// when the test ends.
func StartRTSPServer(t testing.TB, opts RTSPServerOpts) *RTSPServer {
	t.Helper()
	if len(opts.AUs) == 0 {
		t.Fatal("testutil.StartRTSPServer: no access units")
	}
	if opts.FPS <= 0 {
		opts.FPS = 15
	}
	s := &RTSPServer{
		t:           t,
		opts:        opts,
		conns:       map[*gortsplib.ServerConn]struct{}{},
		sessions:    map[*gortsplib.ServerSession]struct{}{},
		pendingPlay: map[*gortsplib.ServerConn]struct{}{},
		started:     make(chan struct{}),
		done:        make(chan struct{}),
	}
	var videoFormat format.Format
	switch opts.Codec {
	case "", "h264":
		ps := firstParamSets(opts.AUs, h264NALType, 7, 8)
		videoFormat = &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: ps[7], PPS: ps[8]}
	case "h265":
		s.h265 = true
		ps := firstParamSets(opts.AUs, h265NALType, 32, 33, 34)
		videoFormat = &format.H265{PayloadTyp: 96, VPS: ps[32], SPS: ps[33], PPS: ps[34]}
	default:
		t.Fatalf("testutil.StartRTSPServer: unknown codec %q", opts.Codec)
	}

	var addr net.Addr
	s.server = &gortsplib.Server{
		Handler:                  &rtspHandler{s: s},
		RTSPAddress:              "127.0.0.1:0",
		DisableRTCPSenderReports: !opts.RTCP,
		Listen: func(network, address string) (net.Listener, error) {
			ln, err := net.Listen(network, address)
			if err == nil {
				addr = ln.Addr()
			}
			return ln, err
		},
	}
	if err := s.server.Start(); err != nil {
		t.Fatalf("testutil.StartRTSPServer: %v", err)
	}
	s.url = fmt.Sprintf("rtsp://%s/stream", addr)

	s.video = &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{videoFormat}}
	desc := &description.Session{Medias: []*description.Media{s.video}}
	if opts.Audio {
		s.audio = &description.Media{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{&format.G711{PayloadTyp: 8, MULaw: false, SampleRate: 8000, ChannelCount: 1}},
		}
		desc.Medias = append(desc.Medias, s.audio)
	}
	s.stream = &gortsplib.ServerStream{Server: s.server, Desc: desc}
	if err := s.stream.Initialize(); err != nil {
		s.server.Close()
		t.Fatalf("testutil.StartRTSPServer: %v", err)
	}

	s.wg.Add(1)
	go s.run()
	t.Cleanup(s.close)
	return s
}

// URL returns the stream URL, rtsp://127.0.0.1:<port>/stream. Any path is
// served.
func (s *RTSPServer) URL() string { return s.url }

// Disconnect drops every current connection and session. The server keeps
// accepting new ones.
func (s *RTSPServer) Disconnect() {
	s.mu.Lock()
	sessions := make([]*gortsplib.ServerSession, 0, len(s.sessions))
	for ss := range s.sessions {
		sessions = append(sessions, ss)
	}
	conns := make([]*gortsplib.ServerConn, 0, len(s.conns))
	for sc := range s.conns {
		conns = append(conns, sc)
	}
	s.mu.Unlock()
	for _, ss := range sessions {
		ss.Close()
	}
	for _, sc := range conns {
		sc.Close()
	}
}

// SetParamSets replaces the SPS and PPS: in the SDP of later DESCRIBEs and
// in-band, where they replace the fixture's parameter sets and are inserted
// before every later IDR. For H.265 the VPS is kept.
func (s *RTSPServer) SetParamSets(sps, pps []byte) {
	sps, pps = bytes.Clone(sps), bytes.Clone(pps)
	s.mu.Lock()
	s.sps, s.pps = sps, pps
	s.mu.Unlock()
	switch f := s.video.Formats[0].(type) {
	case *format.H264:
		f.SPS, f.PPS = sps, pps
	case *format.H265:
		f.SPS, f.PPS = sps, pps
	}
	s.stream.ReloadDesc()
}

func (s *RTSPServer) close() {
	close(s.done)
	s.wg.Wait()
	s.stream.Close()
	s.server.Close()
}

type nalTypeFunc func([]byte) byte

func h264NALType(n []byte) byte { return n[0] & 0x1f }

// firstParamSets returns the first NAL unit of each wanted type.
func firstParamSets(aus []AU, typeOf nalTypeFunc, types ...byte) map[byte][]byte {
	ps := map[byte][]byte{}
	for _, au := range aus {
		for _, n := range au.NALUs {
			for _, want := range types {
				if typeOf(n) == want && ps[want] == nil {
					ps[want] = n
				}
			}
		}
	}
	return ps
}

// withParamSets applies the SetParamSets override to one access unit.
func (s *RTSPServer) withParamSets(au AU) [][]byte {
	s.mu.Lock()
	sps, pps := s.sps, s.pps
	s.mu.Unlock()
	if sps == nil {
		return au.NALUs
	}
	typeOf, spsType, ppsType, vclMax := nalTypeFunc(h264NALType), byte(7), byte(8), byte(5)
	if s.h265 {
		typeOf, spsType, ppsType, vclMax = h265NALType, 33, 34, 31
	}
	out := make([][]byte, 0, len(au.NALUs)+2)
	inserted := false
	for _, n := range au.NALUs {
		typ := typeOf(n)
		if typ == spsType || typ == ppsType {
			continue
		}
		isVCL := typ <= vclMax && (s.h265 || typ >= 1)
		if au.IDR && isVCL && !inserted {
			out = append(out, sps, pps)
			inserted = true
		}
		out = append(out, n)
	}
	return out
}

type packetEncoder interface {
	Encode([][]byte) ([]*rtp.Packet, error)
}

// run replays the access units in real time once the first PLAY lands.
func (s *RTSPServer) run() {
	defer s.wg.Done()
	select {
	case <-s.started:
	case <-s.done:
		return
	}

	var enc packetEncoder
	var err error
	switch f := s.video.Formats[0].(type) {
	case *format.H264:
		enc, err = f.CreateEncoder()
	case *format.H265:
		enc, err = f.CreateEncoder()
	}
	if err != nil {
		s.t.Errorf("testutil.RTSPServer: video encoder: %v", err)
		return
	}
	var audio *audioPacer
	if s.audio != nil {
		audio, err = newAudioPacer(s.audio, s.opts.StartRTPTimestamp)
		if err != nil {
			s.t.Errorf("testutil.RTSPServer: audio encoder: %v", err)
			return
		}
	}

	fps := int64(s.opts.FPS)
	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()
	for i := int64(0); ; i++ {
		if s.opts.Loop || i < int64(len(s.opts.AUs)) {
			au := s.opts.AUs[i%int64(len(s.opts.AUs))]
			pkts, err := enc.Encode(s.withParamSets(au))
			if err != nil {
				s.t.Errorf("testutil.RTSPServer: encode AU %d: %v", i, err)
				return
			}
			ts := s.opts.StartRTPTimestamp + uint32(i*90000/fps)
			if !s.write(s.video, pkts, ts) {
				return
			}
		}
		if audio != nil && !audio.writeUntil(s, (i+1)*8000/fps) {
			return
		}
		select {
		case <-ticker.C:
		case <-s.done:
			return
		}
	}
}

// write stamps and sends packets. It reports false once the stream is closed.
func (s *RTSPServer) write(medi *description.Media, pkts []*rtp.Packet, ts uint32) bool {
	now := time.Now()
	for _, p := range pkts {
		p.Timestamp = ts
		if err := s.stream.WritePacketRTPWithNTP(medi, p, now); err != nil {
			if !errors.As(err, &liberrors.ErrServerStreamClosed{}) {
				s.t.Errorf("testutil.RTSPServer: write RTP: %v", err)
			}
			return false
		}
	}
	return true
}

// audioPacer emits 20 ms PCMA silence packets (160 samples at 8 kHz).
type audioPacer struct {
	medi  *description.Media
	enc   sampleEncoder
	start uint32
	sent  int64 // samples sent so far
}

type sampleEncoder interface {
	Encode([]byte) ([]*rtp.Packet, error)
}

const pcmaSilence = 0xd5

func newAudioPacer(medi *description.Media, start uint32) (*audioPacer, error) {
	enc, err := medi.Formats[0].(*format.G711).CreateEncoder()
	if err != nil {
		return nil, err
	}
	return &audioPacer{medi: medi, enc: enc, start: start}, nil
}

func (a *audioPacer) writeUntil(s *RTSPServer, target int64) bool {
	const frame = 160
	for a.sent+frame <= target {
		pkts, err := a.enc.Encode(bytes.Repeat([]byte{pcmaSilence}, frame))
		if err != nil {
			s.t.Errorf("testutil.RTSPServer: encode audio: %v", err)
			return false
		}
		if !s.write(a.medi, pkts, a.start+uint32(a.sent)) {
			return false
		}
		a.sent += frame
	}
	return true
}

// rtspHandler implements the gortsplib v5 ServerHandler interfaces.
type rtspHandler struct{ s *RTSPServer }

func (h *rtspHandler) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	h.s.mu.Lock()
	h.s.conns[ctx.Conn] = struct{}{}
	h.s.mu.Unlock()
}

func (h *rtspHandler) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	h.s.mu.Lock()
	delete(h.s.conns, ctx.Conn)
	delete(h.s.pendingPlay, ctx.Conn)
	h.s.mu.Unlock()
}

func (h *rtspHandler) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	h.s.mu.Lock()
	h.s.sessions[ctx.Session] = struct{}{}
	h.s.mu.Unlock()
}

func (h *rtspHandler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	h.s.mu.Lock()
	delete(h.s.sessions, ctx.Session)
	h.s.mu.Unlock()
}

func (h *rtspHandler) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, h.s.stream, nil
}

func (h *rtspHandler) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, h.s.stream, nil
}

func (h *rtspHandler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	h.s.mu.Lock()
	h.s.pendingPlay[ctx.Conn] = struct{}{}
	h.s.mu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnResponse runs after the session is marked active but before the PLAY
// response is written, so packets sent from here on reach the new reader:
// the first AU it gets is AU 0 with StartRTPTimestamp.
func (h *rtspHandler) OnResponse(sc *gortsplib.ServerConn, res *base.Response) {
	h.s.mu.Lock()
	_, playing := h.s.pendingPlay[sc]
	delete(h.s.pendingPlay, sc)
	h.s.mu.Unlock()
	if playing && res.StatusCode == base.StatusOK {
		h.s.startOnce.Do(func() { close(h.s.started) })
	}
}
