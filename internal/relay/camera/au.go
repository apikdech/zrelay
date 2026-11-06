package camera

import (
	"bytes"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
)

// Codec describes the video codec of a session and its current parameter
// sets. Kind is "h264" or "h265". VPS is empty for H.264.
type Codec struct {
	Kind          string
	VPS, SPS, PPS []byte
	Width, Height int
}

// Equal reports whether c and o have the same kind, parameter sets and size.
func (c Codec) Equal(o Codec) bool {
	return c.Kind == o.Kind &&
		bytes.Equal(c.VPS, o.VPS) && bytes.Equal(c.SPS, o.SPS) && bytes.Equal(c.PPS, o.PPS) &&
		c.Width == o.Width && c.Height == o.Height
}

// AccessUnit is one picture: its NAL units in decode order, without start
// codes. PTS and DTS are 90 kHz ticks, unwrapped across RTP timestamp wraps;
// DTS starts at 0 for the first IDR of the session. ParamGen counts parameter
// set changes in the session; it matches the latest Codec call. Wall is the
// UTC wall time of the decode timestamp. Bytes is the total NAL unit size.
type AccessUnit struct {
	PTS, DTS int64
	IDR      bool
	NALUs    [][]byte
	ParamGen uint32
	Wall     time.Time
	Bytes    int
}

// AudioPacket is one G.711 RTP payload as the camera sent it. Codec is
// "PCMA" or "PCMU".
type AudioPacket struct {
	Codec     string
	Payload   []byte
	Timestamp uint32
	Seq       uint16
}

// TimeSource says where AccessUnit.Wall comes from.
type TimeSource string

// Time sources (relay-spec §4.2).
const (
	// TimeSourceRTCP maps RTP time to wall time with RTCP sender reports.
	TimeSourceRTCP TimeSource = "rtcp"
	// TimeSourceArrival is the host arrival time of the first packet plus
	// the media time since.
	TimeSourceArrival TimeSource = "arrival"
)

// SessionSink receives what a session produces. Codec is called before the
// first Video and again whenever the parameter sets change. All calls come
// from one goroutine; they must not block for long.
type SessionSink interface {
	Codec(c Codec, ts TimeSource)
	Video(au *AccessUnit)
	Audio(p *AudioPacket)
}

const (
	videoClockRate = 90000
	// timeSourceWait is how much media time after the first IDR the session
	// waits for an RTCP sender report before it settles on arrival time.
	// Access units are held (not dropped) meanwhile.
	timeSourceWait = 2 * videoClockRate
)

func ticksToDuration(t int64) time.Duration {
	return time.Duration(t) * time.Second / videoClockRate
}

// unwrapper extends 32-bit RTP timestamps to int64. Successive timestamps
// must be less than 2^31 ticks (6.6 h at 90 kHz) apart.
type unwrapper struct {
	started bool
	last    uint32
	value   int64
}

func (u *unwrapper) unwrap(ts uint32) int64 {
	if !u.started {
		u.started, u.last, u.value = true, ts, int64(ts)
		return u.value
	}
	u.value += int64(int32(ts - u.last))
	u.last = ts
	return u.value
}

type dtsExtractor interface {
	Extract(au [][]byte, pts int64) (int64, error)
}

// codecRules holds the codec-specific parts of assembly.
type codecRules struct {
	kind         string
	randomAccess func([][]byte) bool
	newDTS       func() dtsExtractor
	// paramSet classifies a NAL unit as VPS ('v'), SPS ('s'), PPS ('p') or
	// none (0).
	paramSet func([]byte) byte
	size     func(sps []byte) (w, h int, ok bool)
}

var h264Rules = codecRules{
	kind:         "h264",
	randomAccess: h264.IsRandomAccess,
	newDTS: func() dtsExtractor {
		d := &h264.DTSExtractor{}
		d.Initialize()
		return d
	},
	paramSet: func(n []byte) byte {
		switch h264.NALUType(n[0] & 0x1f) {
		case h264.NALUTypeSPS:
			return 's'
		case h264.NALUTypePPS:
			return 'p'
		}
		return 0
	},
	size: func(sps []byte) (int, int, bool) {
		var s h264.SPS
		if s.Unmarshal(sps) != nil {
			return 0, 0, false
		}
		return s.Width(), s.Height(), true
	},
}

var h265Rules = codecRules{
	kind:         "h265",
	randomAccess: h265.IsRandomAccess,
	newDTS: func() dtsExtractor {
		d := &h265.DTSExtractor{}
		d.Initialize()
		return d
	},
	paramSet: func(n []byte) byte {
		if len(n) < 2 {
			return 0
		}
		switch h265.NALUType((n[0] >> 1) & 0x3f) {
		case h265.NALUType_VPS_NUT:
			return 'v'
		case h265.NALUType_SPS_NUT:
			return 's'
		case h265.NALUType_PPS_NUT:
			return 'p'
		}
		return 0
	},
	size: func(sps []byte) (int, int, bool) {
		var s h265.SPS
		if s.Unmarshal(sps) != nil {
			return 0, 0, false
		}
		return s.Width(), s.Height(), true
	},
}

// pendingAU is an access unit held while the time source is undecided.
type pendingAU struct {
	au    *AccessUnit
	rtp   uint32 // RTP timestamp (PTS)
	codec *Codec // non-nil when this AU starts a new parameter set generation
}

// assembler turns decoded access units into SessionSink calls: it tracks
// parameter sets, drops units before the first IDR, computes unwrapped
// PTS/DTS and wall time, and decides the time source.
type assembler struct {
	sink  SessionSink
	rules codecRules
	// ntp maps an RTP timestamp to wall time once an RTCP sender report
	// has arrived.
	ntp func(rtp uint32) (time.Time, bool)

	codec Codec
	gen   uint32

	unwrap       unwrapper
	havePrev     bool
	prevRaw      uint32
	prevTicks    int64
	firstArrival time.Time
	firstTicks   int64

	started    bool  // the first IDR has been seen
	base       int64 // unwrapped RTP time of the first IDR
	anchor     time.Time
	dts        dtsExtractor
	needIDR    bool
	haveDTS    bool
	lastDTS    int64
	decided    bool
	timeSource TimeSource
	pending    []pendingAU
}

func newAssembler(rules codecRules, vps, sps, pps []byte, sink SessionSink, ntp func(uint32) (time.Time, bool)) *assembler {
	a := &assembler{sink: sink, rules: rules, ntp: ntp}
	a.codec = a.withParams(Codec{Kind: rules.kind}, vps, sps, pps)
	return a
}

// withParams returns c with the non-nil parameter sets replaced.
func (a *assembler) withParams(c Codec, vps, sps, pps []byte) Codec {
	if vps != nil {
		c.VPS = bytes.Clone(vps)
	}
	if pps != nil {
		c.PPS = bytes.Clone(pps)
	}
	if sps != nil && !bytes.Equal(sps, c.SPS) {
		c.SPS = bytes.Clone(sps)
		c.Width, c.Height = 0, 0
		if w, h, ok := a.rules.size(sps); ok {
			c.Width, c.Height = w, h
		}
	}
	return c
}

// packet records one video RTP packet's timestamp and arrival. It returns
// the RTP timestamp and unwrapped ticks of the access unit the decoder
// completes with this packet, if any: the previous timestamp when the
// timestamp changed, otherwise this one.
func (a *assembler) packet(ts uint32, arrival time.Time) (auRaw uint32, auTicks int64) {
	ticks := a.unwrap.unwrap(ts)
	auRaw, auTicks = ts, ticks
	if !a.havePrev {
		a.firstArrival, a.firstTicks = arrival, ticks
	} else if ts != a.prevRaw {
		auRaw, auTicks = a.prevRaw, a.prevTicks
	}
	a.havePrev, a.prevRaw, a.prevTicks = true, ts, ticks
	return auRaw, auTicks
}

// accessUnit handles one decoded access unit with RTP timestamp raw
// (unwrapped: ticks). nalus must not be retained by the caller.
func (a *assembler) accessUnit(nalus [][]byte, raw uint32, ticks int64) {
	var newCodec *Codec
	if c, changed := a.inBandParams(nalus); changed {
		a.codec = c
		if a.started {
			a.gen++
			newCodec = &c
		}
	}

	idr := a.rules.randomAccess(nalus)
	if !a.started {
		if !idr {
			return // nothing is decodable before the first IDR
		}
		a.started = true
		a.base = ticks
		a.anchor = a.firstArrival.Add(ticksToDuration(ticks - a.firstTicks)).UTC()
		a.needIDR = true
	}
	if a.needIDR {
		if !idr {
			return
		}
		a.dts = a.rules.newDTS()
		a.needIDR = false
	}

	pts := ticks - a.base
	dts, err := a.dts.Extract(a.withParamSets(nalus, idr), pts)
	if err != nil || (a.haveDTS && dts <= a.lastDTS) {
		// Resynchronise at the next IDR rather than emit a non-monotonic DTS.
		a.needIDR = true
		return
	}
	a.haveDTS, a.lastDTS = true, dts

	au := &AccessUnit{PTS: pts, DTS: dts, IDR: idr, NALUs: cloneNALUs(nalus), ParamGen: a.gen}
	for _, n := range au.NALUs {
		au.Bytes += len(n)
	}
	p := pendingAU{au: au, rtp: raw, codec: newCodec}

	if !a.decided {
		if len(a.pending) == 0 {
			c := a.codec // the first Codec call announces the codec in force
			p.codec = &c
		}
		a.pending = append(a.pending, p)
		if _, ok := a.ntp(raw); ok {
			a.decide(TimeSourceRTCP)
		} else if pts >= timeSourceWait {
			a.decide(TimeSourceArrival)
		}
		return
	}
	a.emit(p)
}

// decide fixes the session's time source and flushes the held units.
func (a *assembler) decide(ts TimeSource) {
	a.decided, a.timeSource = true, ts
	for _, p := range a.pending {
		a.emit(p)
	}
	a.pending = nil
}

func (a *assembler) emit(p pendingAU) {
	if p.codec != nil {
		a.sink.Codec(*p.codec, a.timeSource)
	}
	p.au.Wall = a.wall(p)
	a.sink.Video(p.au)
}

func (a *assembler) wall(p pendingAU) time.Time {
	if a.timeSource == TimeSourceRTCP {
		if t, ok := a.ntp(p.rtp); ok {
			return t.Add(-ticksToDuration(p.au.PTS - p.au.DTS)).UTC()
		}
	}
	return a.anchor.Add(ticksToDuration(p.au.DTS))
}

// inBandParams returns the codec updated with the parameter sets carried in
// nalus, and whether it differs from the current one.
func (a *assembler) inBandParams(nalus [][]byte) (Codec, bool) {
	var vps, sps, pps []byte
	for _, n := range nalus {
		switch a.rules.paramSet(n) {
		case 'v':
			vps = n
		case 's':
			sps = n
		case 'p':
			pps = n
		}
	}
	if vps == nil && sps == nil && pps == nil {
		return a.codec, false
	}
	c := a.withParams(a.codec, vps, sps, pps)
	return c, !c.Equal(a.codec)
}

// withParamSets prepends the current parameter sets to a random access unit
// for the DTS extractor, which needs them in-band.
func (a *assembler) withParamSets(nalus [][]byte, idr bool) [][]byte {
	if !idr {
		return nalus
	}
	out := make([][]byte, 0, len(nalus)+3)
	for _, ps := range [][]byte{a.codec.VPS, a.codec.SPS, a.codec.PPS} {
		if len(ps) > 0 {
			out = append(out, ps)
		}
	}
	return append(out, nalus...)
}

// audio forwards one G.711 packet once video has started.
func (a *assembler) audio(p *AudioPacket) {
	if a.decided {
		a.sink.Audio(p)
	}
}

// cloneNALUs copies nalus into one allocation, detaching them from RTP
// packet buffers.
func cloneNALUs(nalus [][]byte) [][]byte {
	n := 0
	for _, b := range nalus {
		n += len(b)
	}
	buf := make([]byte, 0, n)
	out := make([][]byte, len(nalus))
	for i, b := range nalus {
		start := len(buf)
		buf = append(buf, b...)
		out[i] = buf[start:len(buf):len(buf)]
	}
	return out
}
