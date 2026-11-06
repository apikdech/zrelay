package camera

import (
	"bytes"
	"log/slog"
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
// UTC wall time of the decode timestamp; it strictly increases within a
// session (see assembler.emit). Bytes is the total NAL unit size.
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
	// maxTimestampStep is the largest step between the RTP timestamps of two
	// consecutive access units that is believed. A larger one (either way)
	// means the camera restarted its encoder or RTP clock, and the session
	// re-bases its timeline instead of carrying the jump into DTS and Wall.
	maxTimestampStep = 10 * videoClockRate
	// defaultFrameTicks is the DTS step used across a re-base before two
	// access units have set a real one (30 fps).
	defaultFrameTicks = videoClockRate / 30
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
	au     *AccessUnit
	rtp    uint32    // RTP timestamp (PTS)
	anchor time.Time // arrival-mode wall time of DTS 0 when the unit was queued
	codec  *Codec    // non-nil when this AU starts a new parameter set generation
	// rebased marks the unit that opened a new RTP timeline (see rebase).
	rebased bool
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

	// codec holds the parameter sets in force. announced is the codec of
	// the latest Codec call (valid once haveAnnounced), and gen its
	// generation. A change is announced with the next access unit that is
	// actually queued or emitted, so a dropped unit cannot lose it.
	codec         Codec
	announced     Codec
	haveAnnounced bool
	gen           uint32

	unwrap       unwrapper
	havePacket   bool
	firstArrival time.Time
	firstTicks   int64
	lastArrival  time.Time

	started    bool  // the first IDR has been seen
	base       int64 // unwrapped RTP time of the first IDR
	anchor     time.Time
	dts        dtsExtractor
	needIDR    bool
	haveDTS    bool
	lastDTS    int64
	lastPTS    int64 // PTS of the latest accepted unit
	lastTicks  int64 // unwrapped RTP time of the latest accepted unit
	frameTicks int64 // latest DTS step, the duration assumed across a re-base
	decided    bool
	timeSource TimeSource
	pending    []pendingAU

	// discontinuities counts RTP timeline re-bases in this session.
	discontinuities uint64

	// ntpStale is set (in emit order) from a re-based unit until the NTP
	// mapping agrees with the arrival-based time again, i.e. until a sender
	// report on the new clock has arrived. See wall.
	ntpStale bool

	haveWall    bool
	lastWall    time.Time
	lastWallDTS int64
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

// packet records one video RTP packet's timestamp and arrival, in arrival
// order, for unwrapping and the arrival time source.
func (a *assembler) packet(ts uint32, arrival time.Time) {
	ticks := a.unwrap.unwrap(ts)
	if !a.havePacket {
		a.havePacket, a.firstArrival, a.firstTicks = true, arrival, ticks
	}
	a.lastArrival = arrival
}

// ticksOf unwraps the RTP timestamp of an access unit relative to the latest
// packet passed to packet (an access unit may carry an earlier timestamp
// than the packet that completed it).
func (a *assembler) ticksOf(ts uint32) int64 {
	return a.unwrap.value + int64(int32(ts-a.unwrap.last))
}

// accessUnit handles one decoded access unit with RTP timestamp raw
// (unwrapped: ticks). nalus must not be retained by the caller.
func (a *assembler) accessUnit(nalus [][]byte, raw uint32, ticks int64) {
	if c, changed := a.inBandParams(nalus); changed {
		a.codec = c
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
	rebased := a.discontinuous(ticks, pts)
	if rebased {
		pts = a.rebase(ticks)
		if idr {
			a.dts = a.rules.newDTS() // reordering state of the old timeline
		}
	}
	dts, err := a.dts.Extract(a.withParamSets(nalus, idr), pts)
	if err != nil || (a.haveDTS && dts <= a.lastDTS) {
		// Resynchronise at the next IDR rather than emit a non-monotonic DTS.
		a.needIDR = true
		return
	}
	if a.haveDTS {
		a.frameTicks = dts - a.lastDTS
	}
	a.haveDTS, a.lastDTS, a.lastPTS, a.lastTicks = true, dts, pts, ticks

	p := pendingAU{rtp: raw, anchor: a.anchor, codec: a.announcement(), rebased: rebased}
	au := &AccessUnit{PTS: pts, DTS: dts, IDR: idr, NALUs: cloneNALUs(nalus), ParamGen: a.gen}
	for _, n := range au.NALUs {
		au.Bytes += len(n)
	}
	p.au = au

	if !a.decided {
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

// discontinuous reports whether an access unit with unwrapped RTP time ticks
// and PTS pts (on the current timeline) cannot follow the last accepted one:
// its PTS is not above the last DTS (a PTS is never below its own DTS, and
// DTS strictly increases), or the RTP time stepped by more than
// maxTimestampStep either way. This is a camera that restarted its encoder
// or RTP clock mid-session; the unwrapper cannot tell, and a fresh DTS
// extractor would keep rejecting units until the new clock caught up with
// the old one.
func (a *assembler) discontinuous(ticks, pts int64) bool {
	if !a.haveDTS {
		return false
	}
	step := ticks - a.lastTicks
	return pts <= a.lastDTS || step > maxTimestampStep || step < -maxTimestampStep
}

// rebase continues the timeline from the access unit with RTP time ticks as
// if it followed the last accepted unit by one frame: PTS/DTS go on from
// there and, in arrival mode, Wall is re-anchored to the arrival time (emit
// keeps it strictly increasing). It returns the unit's new PTS.
func (a *assembler) rebase(ticks int64) int64 {
	a.discontinuities++
	frame := a.frameTicks
	if frame <= 0 {
		frame = defaultFrameTicks
	}
	next := max(a.lastPTS, a.lastDTS) + frame
	slog.Warn("camera RTP timeline discontinuity; re-based",
		"rtp_step_seconds", float64(ticks-a.lastTicks)/videoClockRate, "count", a.discontinuities)
	a.base = ticks - next
	arrival := a.lastArrival
	if !a.havePacket {
		arrival = a.anchor.Add(ticksToDuration(a.lastDTS + frame))
	}
	a.anchor = arrival.Add(-ticksToDuration(next)).UTC()
	return next
}

// announcement returns the codec to announce with the access unit about to
// be queued, or nil when the latest Codec call still holds. The first call
// announces generation 0; each later change bumps the generation. Parameter
// sets that changed and changed back while units were dropped announce
// nothing.
func (a *assembler) announcement() *Codec {
	if a.haveAnnounced {
		if a.codec.Equal(a.announced) {
			return nil
		}
		a.gen++
	}
	a.announced, a.haveAnnounced = a.codec, true
	c := a.codec
	return &c
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
	// Wall never runs slower than the media clock: in rtcp mode a sender
	// report that moves the NTP mapping back (a camera NTP step or drift
	// correction) would otherwise make Wall step backwards. Wall then keeps
	// advancing by the DTS step until the mapping is ahead again; a forward
	// step is followed at once.
	wall := a.wall(p)
	if a.haveWall {
		if floor := a.lastWall.Add(ticksToDuration(p.au.DTS - a.lastWallDTS)); wall.Before(floor) {
			wall = floor
		}
	}
	a.haveWall, a.lastWall, a.lastWallDTS = true, wall, p.au.DTS
	p.au.Wall = wall
	a.sink.Video(p.au)
}

// wall returns the wall time of p. In rtcp mode that is the NTP mapping,
// except after a re-base: until the camera's first sender report on the new
// clock, the mapping extrapolates the old clock (gortsplib computes
// int32(ts - lastSenderReportTimeRTP)) and puts the new timestamps hours away
// from the real time, and emit would follow a forward step and keep it. Such a
// mapping, further than maxTimestampStep from the arrival-based time, is
// ignored in favour of the arrival-based time (re-anchored by rebase). The
// first mapping that agrees with it ends the wait.
func (a *assembler) wall(p pendingAU) time.Time {
	byArrival := p.anchor.Add(ticksToDuration(p.au.DTS))
	if a.timeSource != TimeSourceRTCP {
		return byArrival
	}
	if p.rebased {
		a.ntpStale = true
	}
	t, ok := a.ntp(p.rtp)
	if !ok {
		return byArrival
	}
	t = t.Add(-ticksToDuration(p.au.PTS - p.au.DTS)).UTC()
	if a.ntpStale {
		if d := t.Sub(byArrival); d > ticksToDuration(maxTimestampStep) || d < -ticksToDuration(maxTimestampStep) {
			return byArrival
		}
		a.ntpStale = false
	}
	return t
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
