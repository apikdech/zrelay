// Package live serves live view over WebRTC with WHEP signaling (design
// §5.1). H.264 access units from a camera.Hub are forwarded without
// re-encoding; G.711 audio is forwarded when the camera sends it.
package live

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/apikdech/zrelay-v2/internal/bearer"
	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

const (
	// defaultViewerQueue is the viewer queue length when Config leaves it 0
	// (design §8.1 defaults.viewer_queue_frames).
	defaultViewerQueue = 64
	// maxOfferBytes bounds a WHEP request body.
	maxOfferBytes = 64 << 10
	// audioFreshness is how long after its last packet a camera still
	// counts as sending audio.
	audioFreshness = 5 * time.Second
	// connectTimeout closes a session whose peer never connects.
	connectTimeout = 30 * time.Second
	// ICE timeouts: a peer silent for iceFailed (no media, RTCP or ICE
	// consent traffic) is gone and its session is closed (design §5.1).
	iceDisconnected = 5 * time.Second
	iceFailed       = 30 * time.Second
	iceKeepAlive    = 2 * time.Second
)

// h264Profiles lists the profile-level-ids we are willing to negotiate.
// Cameras send High, but a browser only has to find one common entry here,
// and each offers a different set (Safari only Constrained High, Firefox
// only Baseline). RTP passes through unchanged, so this does not alter the
// bitstream.
var h264Profiles = []string{"64001f", "640c1f", "4d001f", "42e01f", "42001f"}

// Config configures a Server.
type Config struct {
	// Listen is the signaling listen address (live.listen). Its host is
	// also the address the ICE UDP socket binds.
	Listen string
	// AdvertiseIP is the only ICE host candidate address offered to
	// browsers (live.advertise_ip). Empty advertises the socket's address.
	AdvertiseIP string
	// ICEUDPPort is the single UDP port all media shares (0 picks a free one).
	ICEUDPPort int
	// MaxViewers bounds concurrent sessions in total; 0 disables live view.
	MaxViewers int
	Verifier   *bearer.Verifier
	// Hubs maps camera id to its Hub.
	Hubs map[string]*camera.Hub
	// ViewerQueue is the per-viewer Hub queue in events (0 means 64).
	ViewerQueue int
	Clock       clock.Clock
}

// Server answers WHEP requests and streams Hub output to browsers.
type Server struct {
	cfg  Config
	api  *webrtc.API // nil when live view is disabled
	conn net.PacketConn
	mux  ice.UDPMux

	// created counts pion peer connections ever created.
	created atomic.Uint64

	mu       sync.Mutex
	closed   bool
	sessions map[string]*session
	perCam   map[string]int

	wg       sync.WaitGroup
	watchers []*audioWatcher
}

// NewServer validates cfg, opens the ICE UDP socket and returns a Server.
// With MaxViewers 0 it opens nothing and answers every offer with 503.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Verifier == nil {
		return nil, errors.New("live: Verifier is required")
	}
	if cfg.MaxViewers < 0 {
		return nil, errors.New("live: MaxViewers must be >= 0")
	}
	if cfg.ViewerQueue <= 0 {
		cfg.ViewerQueue = defaultViewerQueue
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real
	}
	s := &Server{cfg: cfg, sessions: map[string]*session{}, perCam: map[string]int{}}
	if cfg.MaxViewers == 0 {
		return s, nil
	}
	api, err := s.newAPI()
	if err != nil {
		return nil, err
	}
	s.api = api
	for id, hub := range cfg.Hubs {
		if hub == nil {
			return nil, fmt.Errorf("live: camera %q has no hub", id)
		}
		s.watchers = append(s.watchers, newAudioWatcher(hub, cfg.Clock))
	}
	return s, nil
}

// newAPI opens the shared UDP socket and builds the pion API.
func (s *Server) newAPI() (*webrtc.API, error) {
	cfg := s.cfg
	host := ""
	if cfg.Listen != "" {
		h, _, err := net.SplitHostPort(cfg.Listen)
		if err != nil {
			return nil, fmt.Errorf("live: listen %q: %w", cfg.Listen, err)
		}
		host = h
	}
	if host == "" {
		host = "0.0.0.0"
	}
	bindIP, err := netip.ParseAddr(host)
	if err != nil {
		return nil, fmt.Errorf("live: listen host %q is not an IP address", host)
	}
	var advertise netip.Addr
	if cfg.AdvertiseIP != "" {
		if advertise, err = netip.ParseAddr(cfg.AdvertiseIP); err != nil {
			return nil, fmt.Errorf("live: advertise_ip %q is not an IP address", cfg.AdvertiseIP)
		}
	}

	m := &webrtc.MediaEngine{}
	for i, profile := range h264Profiles {
		err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile,
			},
			PayloadType: webrtc.PayloadType(102 + i),
		}, webrtc.RTPCodecTypeVideo)
		if err != nil {
			return nil, err
		}
	}
	for _, a := range []struct {
		mime string
		pt   webrtc.PayloadType
	}{{webrtc.MimeTypePCMA, 8}, {webrtc.MimeTypePCMU, 0}} {
		err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: a.mime, ClockRate: 8000, Channels: 1},
			PayloadType:        a.pt,
		}, webrtc.RTPCodecTypeAudio)
		if err != nil {
			return nil, err
		}
	}
	// NACK recovers occasional loss over Wi-Fi. PLI is deliberately
	// unanswered: cameras emit keyframes on their own.
	ir := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(m, ir); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return nil, err
	}

	conn, err := net.ListenPacket("udp4", net.JoinHostPort(bindIP.String(), strconv.Itoa(cfg.ICEUDPPort)))
	if err != nil {
		return nil, fmt.Errorf("live: ICE UDP socket: %w", err)
	}
	s.conn = conn
	s.mux = webrtc.NewICEUDPMux(nil, conn)

	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(s.mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	// Tailscale carries no multicast, and mDNS would only add delay.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetICETimeouts(iceDisconnected, iceFailed, iceKeepAlive)
	if bindIP.IsLoopback() {
		// A socket bound to loopback has no other address to offer.
		se.SetIncludeLoopbackCandidate(true)
	}
	if advertise.IsValid() {
		err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{advertise.String()},
			AsCandidateType: webrtc.ICECandidateTypeHost,
		})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("live: advertise_ip: %w", err)
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

// ICEAddr returns the address of the shared ICE UDP socket, or nil when
// live view is disabled.
func (s *Server) ICEAddr() net.Addr {
	if s.conn == nil {
		return nil
	}
	return s.conn.LocalAddr()
}

// Viewers returns how many sessions camera has, counting those still
// negotiating.
func (s *Server) Viewers(camera string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.perCam[camera]
}

// Close ends every session and releases the ICE socket.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	live := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		live = append(live, sess)
	}
	s.mu.Unlock()
	for _, sess := range live {
		sess.stop()
	}
	for _, w := range s.watchers {
		w.stop()
	}
	s.wg.Wait()
	var err error
	if s.mux != nil {
		err = s.mux.Close()
	}
	if s.conn != nil {
		if cerr := s.conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
			err = cerr
		}
	}
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

// reserve registers a new session for camera, or reports why it cannot.
func (s *Server) reserve(camera, id string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("live view is shutting down")
	}
	if len(s.sessions) >= s.cfg.MaxViewers {
		return nil, errMaxViewers
	}
	sess := &session{srv: s, id: id, camera: camera, done: make(chan struct{})}
	s.sessions[id] = sess
	s.perCam[camera]++
	return sess, nil
}

var errMaxViewers = errors.New("maximum number of viewers reached")

// lookup returns the session id of camera, if any.
func (s *Server) lookup(camera, id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil || sess.camera != camera {
		return nil
	}
	return sess
}

// audioCodec returns "PCMA" or "PCMU" when camera is sending audio, else "".
func (s *Server) audioCodec(camera string) string {
	hub := s.cfg.Hubs[camera]
	for _, w := range s.watchers {
		if w.hub == hub {
			return w.codec()
		}
	}
	return ""
}

func (s *Server) log() *slog.Logger { return slog.Default().With("component", "live") }

// audioWatcher notes which G.711 codec, if any, a camera is currently
// sending, so an offer can be answered with a matching audio track.
type audioWatcher struct {
	hub *camera.Hub
	clk clock.Clock
	sub *camera.Sub

	mu   sync.Mutex
	name string
	seen time.Time

	done chan struct{}
}

func newAudioWatcher(hub *camera.Hub, clk clock.Clock) *audioWatcher {
	w := &audioWatcher{hub: hub, clk: clk, sub: hub.Subscribe(camera.ClassViewer, 256), done: make(chan struct{})}
	go w.run()
	return w
}

func (w *audioWatcher) run() {
	defer close(w.done)
	for ev := range w.sub.C() {
		if ev.Kind != camera.EventAudio {
			continue
		}
		w.mu.Lock()
		w.name, w.seen = ev.Audio.Codec, w.clk.Now()
		w.mu.Unlock()
	}
}

func (w *audioWatcher) codec() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.name == "" || w.clk.Now().Sub(w.seen) > audioFreshness {
		return ""
	}
	return w.name
}

func (w *audioWatcher) stop() {
	w.sub.Close()
	<-w.done
}
