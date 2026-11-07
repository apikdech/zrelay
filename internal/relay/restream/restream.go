// Package restream republishes camera streams over RTSP for players that are
// not browsers (design §5.2, D16). It subscribes to each camera's Hub as a
// viewer, packetizes access units back into RTP, and serves them at
// /{camera} over TCP. Nothing is stored and nothing is re-encoded.
package restream

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

const (
	videoPayloadType = 96
	pcmaPayloadType  = 8
	pcmuPayloadType  = 0

	// defaultViewerQueue is the Hub queue used when Config.ViewerQueue is 0.
	defaultViewerQueue = 256
)

// Config configures a Server.
type Config struct {
	// Listen is the RTSP TCP address, e.g. ":8554". Use "127.0.0.1:0" in tests.
	Listen string
	// Username and Password are the shared restream login. Both are required.
	Username, Password string
	// Hubs maps camera id to its Hub. The id is the URL path: /{camera}.
	Hubs map[string]*camera.Hub
	// ViewerQueue is the size of each camera subscription's queue
	// (default 256). A viewer that falls behind resynchronizes at the next IDR.
	ViewerQueue int
	// ViewerQueues overrides ViewerQueue per camera id (viewer_queue_frames
	// of that camera).
	ViewerQueues map[string]int
}

// viewerQueue is the subscription queue length of camera.
func (s *Server) viewerQueue(camera string) int {
	if n := s.cfg.ViewerQueues[camera]; n > 0 {
		return n
	}
	return s.cfg.ViewerQueue
}

// Server serves the cameras of one Config. It offers TCP only: UDP would need
// more published ports, and tunnels are refused.
type Server struct {
	cfg  Config
	rtsp gortsplib.Server

	addrMu sync.Mutex
	addr   net.Addr

	mu      sync.RWMutex
	streams map[string]*gortsplib.ServerStream  // by camera
	playing map[*gortsplib.ServerSession]string // session -> camera

	pubs      []*publisher
	wg        sync.WaitGroup
	started   bool
	closeOnce sync.Once
}

// New validates cfg and returns a Server that is not yet listening.
func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		return nil, errors.New("restream: listen address required")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("restream: username and password required")
	}
	for name, hub := range cfg.Hubs {
		if name == "" || strings.ContainsAny(name, "/?#% \t\r\n") {
			return nil, fmt.Errorf("restream: invalid camera id %q", name)
		}
		if hub == nil {
			return nil, fmt.Errorf("restream: camera %q has no hub", name)
		}
	}
	if cfg.ViewerQueue <= 0 {
		cfg.ViewerQueue = defaultViewerQueue
	}
	s := &Server{
		cfg:     cfg,
		streams: map[string]*gortsplib.ServerStream{},
		playing: map[*gortsplib.ServerSession]string{},
	}
	s.rtsp = gortsplib.Server{
		RTSPAddress: cfg.Listen,
		Handler:     handler{s},
		Listen: func(network, address string) (net.Listener, error) {
			ln, err := net.Listen(network, address)
			if err == nil {
				s.addrMu.Lock()
				s.addr = ln.Addr()
				s.addrMu.Unlock()
			}
			return ln, err
		},
	}
	return s, nil
}

// Start listens and begins republishing every camera. The caller must Close
// the Server.
func (s *Server) Start() error {
	if s.started {
		return errors.New("restream: already started")
	}
	if err := s.rtsp.Start(); err != nil {
		return err
	}
	s.started = true
	for name, hub := range s.cfg.Hubs {
		p := &publisher{srv: s, name: name, sub: hub.Subscribe(camera.ClassViewer, s.viewerQueue(name))}
		s.pubs = append(s.pubs, p)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			p.run()
		}()
	}
	return nil
}

// Addr returns the address the server listens on, or "" before Start.
func (s *Server) Addr() string {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	if s.addr == nil {
		return ""
	}
	return s.addr.String()
}

// Close drops every client and stops the server. It is safe to call more than
// once.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		for _, p := range s.pubs {
			p.sub.Close()
		}
		s.wg.Wait()
		for _, p := range s.pubs {
			p.unpublish()
		}
		if s.started {
			s.rtsp.Close()
		}
	})
}

// Clients returns how many sessions are playing the camera.
func (s *Server) Clients(camera string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, c := range s.playing {
		if c == camera {
			n++
		}
	}
	return n
}

// publisher turns one camera's Hub events into RTP on a ServerStream.
// Everything but srv and name is touched only by run (and unpublish after run
// has returned).
type publisher struct {
	srv  *Server
	name string
	sub  *camera.Sub

	codec     *camera.Codec
	audioKind string // "PCMA", "PCMU" or "" when the camera has sent no audio
	stream    *gortsplib.ServerStream
	video     *description.Media
	audio     *description.Media
	encode    func(au [][]byte) ([]*rtp.Packet, error)
}

func (p *publisher) run() {
	for ev := range p.sub.C() {
		switch ev.Kind {
		case camera.EventCodec:
			if p.stream != nil && p.codec != nil && p.codec.Equal(*ev.Codec) {
				continue
			}
			c := *ev.Codec
			p.codec = &c
			p.publish()
		case camera.EventVideo:
			p.writeVideo(ev.AU)
		case camera.EventAudio:
			p.writeAudio(ev.Audio)
		case camera.EventLost:
			// Clients are dropped and must reconnect, as with iptv-seer.
			p.codec = nil
			p.audioKind = ""
			p.unpublish()
		}
	}
}

// publish (re)builds the stream for the current codec and audio kind. Clients
// of the previous stream are dropped.
func (p *publisher) publish() {
	c := p.codec
	var vfmt format.Format
	var enc func([][]byte) ([]*rtp.Packet, error)
	switch c.Kind {
	case "h264":
		e := &rtph264.Encoder{PayloadType: videoPayloadType, PacketizationMode: 1}
		if err := e.Init(); err != nil {
			slog.Error("restream: h264 encoder", "camera", p.name, "err", err)
			return
		}
		vfmt = &format.H264{PayloadTyp: videoPayloadType, SPS: c.SPS, PPS: c.PPS, PacketizationMode: 1}
		enc = e.Encode
	case "h265":
		e := &rtph265.Encoder{PayloadType: videoPayloadType}
		if err := e.Init(); err != nil {
			slog.Error("restream: h265 encoder", "camera", p.name, "err", err)
			return
		}
		vfmt = &format.H265{PayloadTyp: videoPayloadType, VPS: c.VPS, SPS: c.SPS, PPS: c.PPS}
		enc = e.Encode
	default:
		slog.Error("restream: unsupported codec", "camera", p.name, "kind", c.Kind)
		return
	}

	video := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{vfmt}}
	desc := &description.Session{Medias: []*description.Media{video}}
	var audio *description.Media
	if p.audioKind != "" {
		audio = &description.Media{
			Type: description.MediaTypeAudio,
			Formats: []format.Format{&format.G711{
				PayloadTyp:   audioPayloadType(p.audioKind),
				MULaw:        p.audioKind == "PCMU",
				SampleRate:   8000,
				ChannelCount: 1,
			}},
		}
		desc.Medias = append(desc.Medias, audio)
	}

	stream := &gortsplib.ServerStream{Server: &p.srv.rtsp, Desc: desc}
	if err := stream.Initialize(); err != nil {
		slog.Error("restream: stream init", "camera", p.name, "err", err)
		return
	}
	p.unpublish()
	p.stream, p.video, p.audio, p.encode = stream, video, audio, enc
	p.srv.mu.Lock()
	p.srv.streams[p.name] = stream
	p.srv.mu.Unlock()
}

// unpublish removes the stream and drops its clients.
func (p *publisher) unpublish() {
	if p.stream == nil {
		return
	}
	p.srv.mu.Lock()
	if p.srv.streams[p.name] == p.stream {
		delete(p.srv.streams, p.name)
	}
	p.srv.mu.Unlock()
	p.stream.Close() // not under srv.mu: closing sessions calls OnSessionClose
	p.stream, p.video, p.audio, p.encode = nil, nil, nil, nil
}

func (p *publisher) writeVideo(au *camera.AccessUnit) {
	if p.stream == nil || au == nil {
		return
	}
	nalus := make([][]byte, 0, len(au.NALUs))
	for _, n := range au.NALUs {
		if len(n) > 0 { // the encoder panics on empty NAL units
			nalus = append(nalus, n)
		}
	}
	if len(nalus) == 0 {
		return
	}
	pkts, err := p.encode(nalus)
	if err != nil {
		slog.Debug("restream: packetize failed", "camera", p.name, "err", err)
		return
	}
	for _, pkt := range pkts {
		pkt.Timestamp = uint32(au.PTS)
		if err := p.stream.WritePacketRTP(p.video, pkt); err != nil {
			slog.Debug("restream: write failed", "camera", p.name, "err", err)
		}
	}
}

func (p *publisher) writeAudio(a *camera.AudioPacket) {
	if p.stream == nil || a == nil || (a.Codec != "PCMA" && a.Codec != "PCMU") {
		return
	}
	if a.Codec != p.audioKind {
		// The camera's first audio packet (or a codec switch): the track list
		// changes, so republish. Clients connected before this are dropped.
		p.audioKind = a.Codec
		p.publish()
		if p.audio == nil {
			return
		}
	}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    audioPayloadType(a.Codec),
			SequenceNumber: a.Seq,
			Timestamp:      a.Timestamp,
		},
		Payload: a.Payload,
	}
	if err := p.stream.WritePacketRTP(p.audio, pkt); err != nil {
		slog.Debug("restream: write failed", "camera", p.name, "err", err)
	}
}

func audioPayloadType(kind string) uint8 {
	if kind == "PCMU" {
		return pcmuPayloadType
	}
	return pcmaPayloadType
}

type handler struct{ s *Server }

func (h handler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return h.find(ctx.Conn, ctx.Request, ctx.Path)
}

func (h handler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return h.find(ctx.Conn, ctx.Request, ctx.Path)
}

func (h handler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	cam := strings.Trim(ctx.Path, "/")
	h.s.mu.Lock()
	h.s.playing[ctx.Session] = cam
	h.s.mu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h handler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	h.s.mu.Lock()
	delete(h.s.playing, ctx.Session)
	h.s.mu.Unlock()
}

// find refuses RTSP tunnelled over HTTP or WebSocket (the library accepts
// WebSockets from any origin, so a web page on a tailnet device could
// otherwise reach the cameras), then checks the login, then looks the camera
// up. It runs for SETUP as well as DESCRIBE, as a client may skip DESCRIBE.
// Auth comes before the 404, so unauthenticated callers cannot probe camera
// ids.
func (h handler) find(conn *gortsplib.ServerConn, req *base.Request, path string) (*base.Response, *gortsplib.ServerStream, error) {
	if conn.Transport().Tunnel != gortsplib.TunnelNone {
		return &base.Response{StatusCode: base.StatusForbidden}, nil, nil
	}
	if !conn.VerifyCredentials(req, h.s.cfg.Username, h.s.cfg.Password) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	st, ok := h.s.streams[strings.Trim(path, "/")]
	if !ok {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}
