package camera

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
)

// rtspTimeout is the RTSP read and write timeout (iptv-seer).
const rtspTimeout = 10 * time.Second

// errNoVideo is returned when the camera offers neither H.264 nor H.265.
var errNoVideo = errors.New("camera: stream has no H.264 or H.265 video")

// RunRTSP plays rawURL over TCP and feeds sink until ctx is done (it then
// returns ctx.Err()) or the session is lost. Returned errors never contain
// the URL's credentials.
func RunRTSP(ctx context.Context, rawURL string, sink SessionSink) error {
	err := runRTSP(ctx, rawURL, sink)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return Redact(err, rawURL)
}

// decoder is the common shape of rtph264.Decoder and rtph265.Decoder.
type decoder interface {
	Decode(pkt *rtp.Packet) ([][]byte, error)
}

func runRTSP(ctx context.Context, rawURL string, sink SessionSink) error {
	u, err := base.ParseURL(rawURL)
	if err != nil {
		return errors.New("camera: invalid RTSP URL")
	}
	// TCP: camera Wi-Fi drops UDP often, and a lost packet corrupts a GOP.
	protocol := gortsplib.ProtocolTCP
	c := &gortsplib.Client{
		Scheme:       u.Scheme,
		Host:         u.Host,
		Protocol:     &protocol,
		ReadTimeout:  rtspTimeout,
		WriteTimeout: rtspTimeout,
	}
	if err := c.Start(); err != nil {
		return err
	}
	var closeOnce sync.Once
	closeClient := func() { closeOnce.Do(c.Close) }
	defer closeClient()
	defer context.AfterFunc(ctx, closeClient)()

	desc, _, err := c.Describe(u)
	if err != nil {
		return err
	}

	videoMedia, videoFormat, a, dec, err := setupVideo(c, desc, sink)
	if err != nil {
		return err
	}
	video := newVideoPath(a, dec)
	c.OnPacketRTP(videoMedia, videoFormat, func(pkt *rtp.Packet) {
		video.packet(pkt, time.Now())
	})

	if err := setupAudio(c, desc, a); err != nil {
		return err
	}

	if _, err := c.Play(nil); err != nil {
		return err
	}
	return c.Wait()
}

// videoPath feeds video RTP packets through the depacketizer into the
// assembler.
type videoPath struct {
	a   *assembler
	dec depacketizer
}

// newVideoPath wraps dec (an *rtph264.Decoder or *rtph265.Decoder) so each
// access unit it completes carries the RTP timestamp the decoder used.
func newVideoPath(a *assembler, dec decoder) *videoPath {
	v := &videoPath{a: a, dec: markerDepacketizer{dec}}
	if d, ok := dec.(*rtph264.Decoder); ok {
		v.dec = &h264Depacketizer{d: d}
	}
	return v
}

// packet handles one video RTP packet that arrived at arrival.
func (v *videoPath) packet(pkt *rtp.Packet, arrival time.Time) {
	v.a.packet(pkt.Timestamp, arrival)
	nalus, ts, err := v.dec.decode(pkt)
	if err != nil || len(nalus) == 0 {
		return // more packets needed, or a damaged unit
	}
	v.a.accessUnit(nalus, ts, v.a.ticksOf(ts))
}

// depacketizer returns the access unit, if any, that a packet completes and
// that access unit's RTP timestamp.
type depacketizer interface {
	decode(pkt *rtp.Packet) (nalus [][]byte, ts uint32, err error)
}

// markerDepacketizer is for decoders that complete an access unit only on a
// packet with the marker bit, so the unit has that packet's timestamp
// (rtph265.Decoder in gortsplib v5.6.7).
type markerDepacketizer struct{ d decoder }

func (m markerDepacketizer) decode(pkt *rtp.Packet) ([][]byte, uint32, error) {
	nalus, err := m.d.Decode(pkt)
	return nalus, pkt.Timestamp, err
}

// h264Depacketizer tracks the frame buffered inside rtph264.Decoder, which
// does not report the timestamp of the access units it returns. Mirroring
// Decoder.Decode in gortsplib v5.6.7, a packet that yields complete NAL
// units either
//   - has a new timestamp while an earlier frame is buffered (a camera that
//     omits the marker bit): the buffered frame is returned with its own
//     timestamp and the new NAL units are buffered, marker or not; or
//   - is added to the buffer, which is returned with this packet's
//     timestamp when the marker bit is set.
//
// A packet holding only the start or middle of a fragmented NAL unit leaves
// the buffer as it was.
type h264Depacketizer struct {
	d        *rtph264.Decoder
	buffered bool   // the decoder holds NAL units of an unfinished frame
	bufTS    uint32 // their RTP timestamp
}

func (h *h264Depacketizer) decode(pkt *rtp.Packet) ([][]byte, uint32, error) {
	complete := h264CompletesNALU(pkt.Payload)
	split := complete && h.buffered && pkt.Timestamp != h.bufTS
	ts := pkt.Timestamp
	if split {
		ts = h.bufTS
	}
	nalus, err := h.d.Decode(pkt)
	switch {
	case !complete:
		// Fragment start or middle: the frame buffer is untouched.
	case err != nil && !errors.Is(err, rtph264.ErrMorePacketsNeeded):
		// A damaged packet: the decoder rejected it before buffering.
	case split:
		h.buffered, h.bufTS = true, pkt.Timestamp
	case pkt.Marker:
		// The buffer was returned (or dropped as a trailing SEI unit).
		h.buffered = false
	default:
		h.buffered, h.bufTS = true, pkt.Timestamp
	}
	return nalus, ts, err
}

// h264CompletesNALU reports whether an H.264 RTP payload ends with complete
// NAL units: a single NAL unit, a STAP-A, or the last fragment of an FU-A.
func h264CompletesNALU(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch h264.NALUType(payload[0] & 0x1f) {
	case h264.NALUTypeFUA:
		return len(payload) >= 2 && payload[1]&0x40 != 0
	case h264.NALUTypeSTAPB, h264.NALUTypeMTAP16, h264.NALUTypeMTAP24, h264.NALUTypeFUB:
		return false // unsupported by the decoder
	}
	return true
}

// setupVideo sets up the selected video track.
func setupVideo(c *gortsplib.Client, desc *description.Session, sink SessionSink) (*description.Media, format.Format, *assembler, decoder, error) {
	medi, forma, err := selectVideo(desc)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var (
		dec decoder
		a   *assembler
	)
	switch f := forma.(type) {
	case *format.H264:
		dec, err = f.CreateDecoder()
		a = newAssembler(h264Rules, nil, f.SPS, f.PPS, sink, ntpFunc(c, medi, f))
	case *format.H265:
		dec, err = f.CreateDecoder()
		a = newAssembler(h265Rules, f.VPS, f.SPS, f.PPS, sink, ntpFunc(c, medi, f))
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return nil, nil, nil, nil, err
	}
	return medi, forma, a, dec, nil
}

// selectVideo picks the H.264 track, or else the H.265 one.
func selectVideo(desc *description.Session) (*description.Media, format.Format, error) {
	var h264f *format.H264
	if medi := desc.FindFormat(&h264f); medi != nil {
		return medi, h264f, nil
	}
	var h265f *format.H265
	if medi := desc.FindFormat(&h265f); medi != nil {
		return medi, h265f, nil
	}
	return nil, nil, errNoVideo
}

// ntpFunc maps RTP timestamps of a video format to wall time using the RTCP
// sender reports gortsplib has received.
func ntpFunc(c *gortsplib.Client, medi *description.Media, forma format.Format) func(uint32) (time.Time, bool) {
	pt := forma.PayloadType()
	return func(ts uint32) (time.Time, bool) {
		return c.PacketNTP(medi, &rtp.Packet{Header: rtp.Header{PayloadType: pt, Timestamp: ts}})
	}
}

// setupAudio sets up G.711 8 kHz mono audio when the camera offers it.
func setupAudio(c *gortsplib.Client, desc *description.Session, a *assembler) error {
	medi, g711, codec := selectAudio(desc)
	if medi == nil {
		return nil
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return err
	}
	c.OnPacketRTP(medi, g711, func(pkt *rtp.Packet) {
		a.audio(&AudioPacket{
			Codec:     codec,
			Payload:   bytes.Clone(pkt.Payload),
			Timestamp: pkt.Timestamp,
			Seq:       pkt.SequenceNumber,
		})
	})
	return nil
}

// selectAudio picks a G.711 8 kHz mono track and names its codec ("PCMA" or
// "PCMU"). It returns a nil media when there is none.
func selectAudio(desc *description.Session) (*description.Media, *format.G711, string) {
	var g711 *format.G711
	medi := desc.FindFormat(&g711)
	if medi == nil || g711.SampleRate != 8000 || g711.ChannelCount != 1 {
		return nil, nil, ""
	}
	if g711.MULaw {
		return medi, g711, "PCMU"
	}
	return medi, g711, "PCMA"
}

// Redact removes the credentials of rawURL from err's message: the userinfo
// where it is followed by '@' (as written in rawURL and as net/url prints
// it), and the password in raw and escaped form. A bare username elsewhere
// in the message is left alone. It returns nil for nil and err itself when
// the message holds no credentials.
//
// A redacted error still matches its causes with errors.Is, but it does not
// unwrap: errors.Unwrap and errors.As would hand out the original error,
// whose message holds the credentials.
func Redact(err error, rawURL string) error {
	if err == nil {
		return nil
	}
	u, perr := url.Parse(rawURL)
	if perr != nil {
		// Credentials cannot be located, so none of the message is kept.
		return &redactedError{msg: "camera: RTSP session failed (message withheld: unparseable URL)", cause: err}
	}
	if u.User == nil {
		return err
	}
	type secret struct{ s, with string }
	secrets := []secret{{u.User.String() + "@", "***@"}}
	if raw := rawUserinfo(rawURL); raw != "" {
		secrets = append(secrets, secret{raw + "@", "***@"})
	}
	if pw, ok := u.User.Password(); ok && pw != "" {
		for _, s := range []string{pw, url.PathEscape(pw), url.QueryEscape(pw)} {
			secrets = append(secrets, secret{s, "***"})
		}
	}
	msg := err.Error()
	redacted := msg
	for _, s := range secrets {
		redacted = strings.ReplaceAll(redacted, s.s, s.with)
	}
	if redacted == msg {
		return err
	}
	return &redactedError{msg: redacted, cause: err}
}

// rawUserinfo returns the userinfo of rawURL as written, or "".
func rawUserinfo(rawURL string) string {
	_, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		return rest[:i]
	}
	return ""
}

// redactedError is an error whose message had credentials removed.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }

// Is matches the causes of the original error without exposing it.
func (e *redactedError) Is(target error) bool { return errors.Is(e.cause, target) }
