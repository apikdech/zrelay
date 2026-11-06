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
	c.OnPacketRTP(videoMedia, videoFormat, func(pkt *rtp.Packet) {
		raw, ticks := a.packet(pkt.Timestamp, time.Now())
		nalus, err := dec.Decode(pkt)
		if err != nil || len(nalus) == 0 {
			return // more packets needed, or a damaged unit
		}
		a.accessUnit(nalus, raw, ticks)
	})

	if err := setupAudio(c, desc, a); err != nil {
		return err
	}

	if _, err := c.Play(nil); err != nil {
		return err
	}
	return c.Wait()
}

// setupVideo sets up the H.264 track, or else the H.265 one.
func setupVideo(c *gortsplib.Client, desc *description.Session, sink SessionSink) (*description.Media, format.Format, *assembler, decoder, error) {
	var h264f *format.H264
	if medi := desc.FindFormat(&h264f); medi != nil {
		dec, err := h264f.CreateDecoder()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
			return nil, nil, nil, nil, err
		}
		sps, pps := h264f.SPS, h264f.PPS
		a := newAssembler(h264Rules, nil, sps, pps, sink, ntpFunc(c, medi, h264f))
		return medi, h264f, a, dec, nil
	}
	var h265f *format.H265
	if medi := desc.FindFormat(&h265f); medi != nil {
		dec, err := h265f.CreateDecoder()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
			return nil, nil, nil, nil, err
		}
		vps, sps, pps := h265f.VPS, h265f.SPS, h265f.PPS
		a := newAssembler(h265Rules, vps, sps, pps, sink, ntpFunc(c, medi, h265f))
		return medi, h265f, a, dec, nil
	}
	return nil, nil, nil, nil, errNoVideo
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
	var g711 *format.G711
	medi := desc.FindFormat(&g711)
	if medi == nil || g711.SampleRate != 8000 || g711.ChannelCount != 1 {
		return nil
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return err
	}
	codec := "PCMA"
	if g711.MULaw {
		codec = "PCMU"
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

// Redact removes the credentials of rawURL (userinfo, and the password in
// raw and escaped form) from err's message. It returns nil for nil and err
// itself when the message holds no credentials.
func Redact(err error, rawURL string) error {
	if err == nil {
		return nil
	}
	u, perr := url.Parse(rawURL)
	if perr != nil {
		// Credentials cannot be located, so none of the message is kept.
		return errors.New("camera: RTSP session failed (message withheld: unparseable URL)")
	}
	if u.User == nil {
		return err
	}
	secrets := []string{u.User.String()}
	if pw, ok := u.User.Password(); ok && pw != "" {
		secrets = append(secrets, pw, url.PathEscape(pw), url.QueryEscape(pw))
	}
	msg := err.Error()
	redacted := msg
	for _, s := range secrets {
		if s != "" {
			redacted = strings.ReplaceAll(redacted, s, "***")
		}
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}
