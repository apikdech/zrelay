package live

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/pion/webrtc/v4"
)

// gatherTimeout bounds ICE candidate gathering for the non-trickle answer.
const gatherTimeout = 5 * time.Second

// negotiateHook, when set by a test, runs synchronously at the points of a
// WHEP negotiation where a server Close can arrive: "created" (peer
// connection exists, not yet attached to the session), "attached" and
// "negotiated" (answer ready, response not yet sent). It is nil in
// production.
var negotiateHook func(point string, pc *webrtc.PeerConnection)

func hook(point string, pc *webrtc.PeerConnection) {
	if h := negotiateHook; h != nil {
		h(point, pc)
	}
}

// Handler returns the WHEP endpoints (RFC 9725):
//
//	POST   /live/{camera}            application/sdp offer -> 201 + answer, Location
//	DELETE /live/{camera}/{session}  -> 204
//
// Every request needs the K_api bearer credential.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /live/{camera}", s.handleOffer)
	mux.HandleFunc("DELETE /live/{camera}/{session}", s.handleDelete)
	return mux
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Verifier.Check(r.Header.Get("Authorization")) {
		return true
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	camID := r.PathValue("camera")
	hub := s.cfg.Hubs[camID]
	if hub == nil {
		http.Error(w, "unknown camera", http.StatusNotFound)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/sdp" {
		http.Error(w, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOfferBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "offer too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "cannot read offer", http.StatusBadRequest)
		}
		return
	}
	if s.api == nil {
		http.Error(w, "live view is disabled", http.StatusServiceUnavailable)
		return
	}
	if c, _, ok := hub.CurrentCodec(); ok && c.Kind != "h264" {
		http.Error(w, "live view carries H.264 only", http.StatusNotImplemented)
		return
	}

	id, err := newSessionID()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sess, err := s.reserve(camID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	answer, status, err := s.negotiate(sess, string(body))
	if err != nil {
		if sess.isStopped() {
			// Close ran while we negotiated: a shutdown, not a bad offer.
			status, err = http.StatusServiceUnavailable, errShuttingDown
		}
		sess.stop()
		s.log().Warn("WHEP negotiation failed", "camera", camID, "err", err)
		http.Error(w, "negotiation failed", status)
		return
	}
	if sess.isStopped() {
		sess.stop()
		http.Error(w, errShuttingDown.Error(), http.StatusServiceUnavailable)
		return
	}
	go sess.watchConnect(s.cfg.Clock.NewTimer(connectTimeout))
	s.log().Info("offer answered", "camera", camID, "session", id)

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/live/"+url.PathEscape(camID)+"/"+id)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, answer)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	sess := s.lookup(r.PathValue("camera"), r.PathValue("session"))
	if sess == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	sess.stop()
	w.WriteHeader(http.StatusNoContent)
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// negotiate creates the peer connection of sess, answers offer and returns
// the answer SDP with every candidate in it (no trickle). The HTTP status
// is meaningful only with a non-nil error.
func (s *Server) negotiate(sess *session, offer string) (string, int, error) {
	s.created.Add(1)
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	hook("created", pc)
	if !sess.attach(pc) {
		_ = pc.Close()
		return "", http.StatusServiceUnavailable, errShuttingDown
	}
	hook("attached", pc)

	// No fmtp: the track binds to whichever H.264 entry the browser accepted.
	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "camera")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if err := addTrack(pc, videoTrack); err != nil {
		return "", http.StatusInternalServerError, err
	}
	var audio sampleWriter
	audioCodec := s.audioCodec(sess.camera)
	if audioCodec != "" {
		track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/" + audioCodec, ClockRate: 8000, Channels: 1}, "audio", "camera")
		if err != nil {
			return "", http.StatusInternalServerError, err
		}
		if err := addTrack(pc, track); err != nil {
			return "", http.StatusInternalServerError, err
		}
		audio = track
	}

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.log().Info("viewer connected", "camera", sess.camera, "session", sess.id)
			sess.start(videoTrack, audio, audioCodec)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			sess.stop()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("invalid offer: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", http.StatusInternalServerError, err
	}
	gatherTimer := s.cfg.Clock.NewTimer(gatherTimeout)
	defer gatherTimer.Stop()
	select {
	case <-gathered:
	case <-gatherTimer.C():
		return "", http.StatusInternalServerError, errors.New("ICE gathering timed out")
	}
	hook("negotiated", pc)
	return pc.LocalDescription().SDP, 0, nil
}

func addTrack(pc *webrtc.PeerConnection, track webrtc.TrackLocal) error {
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	go func() {
		// Interceptors only see RTCP (NACKs) if it is read.
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	return nil
}
