package live

import (
	"bytes"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

const (
	// defaultVideoDuration is the duration of a sample with no usable DTS
	// delta (first after start or loss): one frame at 15 fps.
	defaultVideoDuration = time.Second * 6000 / 90000
	// defaultAudioDuration is one 20 ms G.711 packet.
	defaultAudioDuration = 20 * time.Millisecond
	maxSampleDuration    = time.Second
)

var annexBStart = []byte{0, 0, 0, 1}

// sampleWriter is the part of webrtc.TrackLocalStaticSample a viewer uses.
type sampleWriter interface {
	WriteSample(media.Sample) error
}

// viewer turns one Hub subscription into track samples. It does not know
// about pion peer connections, so it can be tested alone.
type viewer struct {
	sub        *camera.Sub
	video      sampleWriter
	audio      sampleWriter // nil when no audio track was negotiated
	audioCodec string

	codec   *camera.Codec
	haveDTS bool
	lastDTS int64
	haveTS  bool
	lastTS  uint32
}

func newViewer(sub *camera.Sub, video, audio sampleWriter, audioCodec string) *viewer {
	return &viewer{sub: sub, video: video, audio: audio, audioCodec: audioCodec}
}

// run consumes the subscription until it is closed. Write errors only mean
// the peer is leaving; the connection state callback ends the session.
func (v *viewer) run() {
	for ev := range v.sub.C() {
		switch ev.Kind {
		case camera.EventCodec:
			v.codec = ev.Codec
		case camera.EventLost:
			// The Hub resumes this subscriber at an IDR with a fresh
			// session or after a lag; DTS may restart from 0.
			v.haveDTS = false
			v.haveTS = false
		case camera.EventVideo:
			v.writeVideo(ev.AU)
		case camera.EventAudio:
			v.writeAudio(ev.Audio)
		}
	}
}

func (v *viewer) writeVideo(au *camera.AccessUnit) {
	if v.codec != nil && v.codec.Kind != "h264" {
		return // browsers get H.264 only
	}
	dur := defaultVideoDuration
	if v.haveDTS {
		if d := au.DTS - v.lastDTS; d > 0 {
			if t := time.Duration(d) * time.Second / 90000; t <= maxSampleDuration {
				dur = t
			}
		}
	}
	v.haveDTS, v.lastDTS = true, au.DTS
	_ = v.video.WriteSample(media.Sample{Data: v.annexB(au), Duration: dur})
}

// annexB encodes au as one Annex-B access unit. An IDR gets the codec's
// SPS and PPS first when the camera did not send them in-band.
func (v *viewer) annexB(au *camera.AccessUnit) []byte {
	var sps, pps []byte
	if au.IDR && v.codec != nil {
		sps, pps = v.codec.SPS, v.codec.PPS
		for _, n := range au.NALUs {
			if len(n) == 0 {
				continue
			}
			switch n[0] & 0x1f {
			case 7:
				sps = nil
			case 8:
				pps = nil
			}
		}
	}
	size := len(sps) + len(pps)
	for _, n := range au.NALUs {
		size += len(annexBStart) + len(n)
	}
	size += 2 * len(annexBStart)
	var b bytes.Buffer
	b.Grow(size)
	for _, n := range [][]byte{sps, pps} {
		if len(n) > 0 {
			b.Write(annexBStart)
			b.Write(n)
		}
	}
	for _, n := range au.NALUs {
		b.Write(annexBStart)
		b.Write(n)
	}
	return b.Bytes()
}

func (v *viewer) writeAudio(p *camera.AudioPacket) {
	if v.audio == nil || p.Codec != v.audioCodec {
		return
	}
	dur := defaultAudioDuration
	if v.haveTS {
		if d := p.Timestamp - v.lastTS; d > 0 && d <= 8000 {
			dur = time.Duration(d) * time.Second / 8000
		}
	}
	v.haveTS, v.lastTS = true, p.Timestamp
	_ = v.audio.WriteSample(media.Sample{Data: p.Payload, Duration: dur})
}

// session is one WHEP viewer: a peer connection and, once connected, a
// Hub subscription served by one goroutine.
type session struct {
	srv    *Server
	id     string
	camera string
	done   chan struct{}

	mu      sync.Mutex
	pc      *webrtc.PeerConnection // set by attach; guarded by mu
	stopped bool
	started bool
	sub     *camera.Sub
	once    sync.Once
}

// start subscribes to the camera and begins forwarding. It runs once the
// peer is connected, so the viewer's first frame is the next IDR.
func (sess *session) start(video, audio sampleWriter, audioCodec string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.stopped || sess.started {
		return
	}
	sess.started = true
	hub := sess.srv.cfg.Hubs[sess.camera]
	sess.sub = hub.Subscribe(camera.ClassViewer, sess.srv.cfg.ViewerQueue)
	v := newViewer(sess.sub, video, audio, audioCodec)
	sess.srv.wg.Add(1)
	go func() {
		defer sess.srv.wg.Done()
		v.run()
	}()
}

// stop ends the session. It is safe to call from any goroutine, including
// pion callbacks, and more than once.
func (sess *session) stop() {
	sess.once.Do(func() {
		s := sess.srv
		s.wg.Add(1)
		s.mu.Lock()
		delete(s.sessions, sess.id)
		s.perCam[sess.camera]--
		if s.perCam[sess.camera] <= 0 {
			delete(s.perCam, sess.camera)
		}
		s.mu.Unlock()

		sess.mu.Lock()
		sess.stopped = true
		sub := sess.sub
		pc := sess.pc // attach refuses a pc after this point; its caller closes it
		sess.mu.Unlock()
		close(sess.done)
		if sub != nil {
			sub.Close()
		}
		// pc.Close waits for pion callbacks, one of which may be calling us.
		go func() {
			defer s.wg.Done()
			if pc != nil {
				_ = pc.Close()
			}
		}()
	})
}

// attach records pc as the session's peer connection. It reports false when
// the session was already stopped, in which case the caller owns pc.
func (sess *session) attach(pc *webrtc.PeerConnection) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.stopped {
		return false
	}
	sess.pc = pc
	return true
}

func (sess *session) isStopped() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.stopped
}

// watchConnect closes the session if the peer never connects. The caller
// creates t, so the deadline is fixed before the answer is sent.
func (sess *session) watchConnect(t clock.Timer) {
	defer t.Stop()
	select {
	case <-t.C():
		sess.mu.Lock()
		started := sess.started
		sess.mu.Unlock()
		if !started {
			sess.srv.log().Info("peer never connected; closing", "camera", sess.camera, "session", sess.id)
			sess.stop()
		}
	case <-sess.done:
	}
}
