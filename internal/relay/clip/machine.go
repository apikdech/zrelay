// Package clip is the per-camera clip lifecycle state machine (relay-spec
// §4.5): it decides when a clip opens, how it is split and why it ends, and
// hands whole GOPs to a ClipWriter. It does no I/O of its own.
package clip

import (
	"crypto/rand"
	"io"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
)

// EndReason says why a clip ended (Contract §6.2 end_reason).
type EndReason string

// End reasons.
const (
	ReasonPostRollElapsed EndReason = "post_roll_elapsed"
	ReasonMaxDuration     EndReason = "max_duration"
	ReasonStreamLost      EndReason = "stream_lost"
	ReasonCodecChange     EndReason = "codec_change"
	ReasonBacklogOverflow EndReason = "backlog_overflow"
	ReasonShutdown        EndReason = "shutdown"
	ReasonRecovered       EndReason = "recovered"
)

// OpenInfo describes a clip (or one segment of a series) being opened.
// ClipID's timestamp is Start in milliseconds. PreRollMs is 0 for
// continuation segments.
type OpenInfo struct {
	ClipID, SeriesID ulid.ULID
	SegmentIndex     int
	Start            time.Time
	PreRollMs        int64
	Codec            camera.Codec
	TimeSource       camera.TimeSource
}

// FinalInfo completes a clip. End is the end of the last frame written.
type FinalInfo struct {
	End        time.Time
	DurationMs int64
	Reason     EndReason
	Triggers   []TriggerRecord
}

// ClipWriter receives one clip. WriteGOP gets whole, closed GOPs in order
// (the final one may be cut short by the end of the clip). The Machine never
// calls it after Finalize or Abort. Abort discards a clip that has no
// fragment (nothing was ever written to it); a clip with fragments always
// ends with Finalize, also when a WriteGOP error forces backlog_overflow.
type ClipWriter interface {
	WriteGOP(g *ring.GOP) error
	Finalize(f FinalInfo) error
	Abort(reason EndReason)
}

// Output opens a ClipWriter per clip segment.
type Output interface {
	Open(o OpenInfo) (ClipWriter, error)
}

// Config configures a Machine.
type Config struct {
	Camera string
	// PostRoll is how long a clip keeps recording after every trigger kind
	// went inactive. MaxClip splits a clip at the next IDR once it is at
	// least this long (0 disables the split).
	PostRoll, MaxClip time.Duration
	// Rand is the entropy for ULIDs; nil means crypto/rand.
	Rand io.Reader
}

// States reported by State.
const (
	stateIdle int32 = iota
	stateRecording
	statePostRoll
	stateSuppressed
	stateClosed
)

var stateNames = [...]string{"Idle", "Recording", "PostRoll", "Suppressed", "Closed"}

// segment is the clip currently being written.
type segment struct {
	w         ClipWriter
	info      OpenInfo
	fragments int       // GOPs written
	fragEnd   time.Time // end of the last written GOP
	pend      []*camera.AccessUnit
}

// continuation is how the next segment of a series is opened after a split
// the machine did not make at an IDR (codec change, failed Open).
type continuation struct {
	series ulid.ULID
	next   int
}

// Machine is the clip state machine of one camera. Every input is called
// from the camera's recorder goroutine; only State is safe from elsewhere.
//
// OnVideo also feeds the pre-roll ring, so the recorder must not push to it.
// The Machine holds the open GOP itself and writes a GOP to the clip when the
// next IDR closes it, or when the clip ends. It returns ring GOPs to the
// writer by reference.
type Machine struct {
	cfg  Config
	clk  clock.Clock
	ring *ring.Ring
	out  Output
	rnd  io.Reader

	state atomic.Int32

	triggered bool
	trig      trigSet
	cur       *segment
	cont      *continuation
	deadline  time.Time // post-roll end, in PostRoll

	codec     camera.Codec
	haveCodec bool
	ts        camera.TimeSource

	paramGen  uint32
	haveGen   bool
	lastWall  time.Time
	frameDur  time.Duration // estimate of one frame's duration
	haveFrame bool
}

// New returns an idle Machine reading pre-roll from r and writing clips to out.
func New(cfg Config, clk clock.Clock, r *ring.Ring, out Output) *Machine {
	rnd := cfg.Rand
	if rnd == nil {
		rnd = rand.Reader
	}
	return &Machine{cfg: cfg, clk: clk, ring: r, out: out, rnd: rnd}
}

// State returns "Idle", "Recording", "PostRoll", "Suppressed", or "Closed"
// after Shutdown.
func (m *Machine) State() string { return stateNames[m.state.Load()] }

func (m *Machine) setState(s int32) { m.state.Store(s) }

func (m *Machine) active() bool { return m.cur != nil }

// OnCodec records the session's codec and time source. A codec that differs
// from the previous one ends the active clip with codec_change.
func (m *Machine) OnCodec(c camera.Codec, ts camera.TimeSource) {
	if m.closed() {
		return
	}
	changed := m.haveCodec && !c.Equal(m.codec)
	m.codec, m.haveCodec, m.ts = c, true, ts
	if changed {
		m.codecChanged()
	}
}

// OnVideo takes one access unit: into the ring, and into the active clip.
func (m *Machine) OnVideo(au *camera.AccessUnit) {
	if m.closed() {
		return
	}
	if m.haveGen && au.ParamGen != m.paramGen {
		m.codecChanged()
	}
	m.paramGen, m.haveGen = au.ParamGen, true
	if m.haveFrame {
		if d := au.Wall.Sub(m.lastWall); d > 0 {
			m.frameDur = d
		}
	}
	m.lastWall, m.haveFrame = au.Wall, true

	m.ring.Push(au)
	if m.active() {
		m.clipAU(au)
		return
	}
	// A failed Open is retried at the next IDR, not on every frame.
	if au.IDR && m.state.Load() == stateIdle {
		m.tryOpen()
	}
}

// OnLost reports that the RTSP session was lost. The active clip ends with
// stream_lost after writing the frames received so far, including those of
// the open GOP (relay-spec 4.5). Only a clip that holds no frame at all is
// aborted.
func (m *Machine) OnLost() {
	if m.closed() {
		return
	}
	if m.active() {
		m.finish(ReasonStreamLost, true, stateIdle)
	}
	m.ring.Reset()
	m.cont = nil
	m.haveGen, m.haveFrame = false, false
}

// OnSignal feeds trigger bookkeeping for the sidecar. It does not decide
// whether a clip is open; OnTriggered does.
func (m *Machine) OnSignal(s events.Signal) {
	if m.closed() {
		return
	}
	m.trig.apply(s, m.active())
}

// OnTriggered reports whether any trigger kind is active.
func (m *Machine) OnTriggered(active bool) {
	if m.closed() {
		return
	}
	m.triggered = active
	switch m.state.Load() {
	case stateIdle:
		if active {
			m.tryOpen()
		} else {
			m.cont = nil
		}
	case stateRecording:
		if !active {
			m.deadline = m.clk.Now().Add(m.cfg.PostRoll)
			m.setState(statePostRoll)
		}
	case statePostRoll:
		if active {
			m.setState(stateRecording)
		}
	}
}

// Tick ends a clip whose post-roll has elapsed. Call it regularly.
func (m *Machine) Tick() {
	if m.state.Load() != statePostRoll || m.clk.Now().Before(m.deadline) {
		return
	}
	if !m.active() {
		m.setState(stateIdle)
		return
	}
	m.finish(ReasonPostRollElapsed, true, stateIdle)
}

// ForceFinalize ends the active clip with reason and suppresses new clips
// until Unsuppress (the egress backlog policy, relay-spec §4.8).
func (m *Machine) ForceFinalize(reason EndReason) {
	if m.closed() {
		return
	}
	if m.active() {
		m.finish(reason, true, stateSuppressed)
	}
	m.cont = nil
	m.setState(stateSuppressed)
}

// Unsuppress leaves Suppressed. A trigger still held opens a clip at once.
func (m *Machine) Unsuppress() {
	if m.state.Load() != stateSuppressed {
		return
	}
	m.setState(stateIdle)
	m.tryOpen()
}

// Shutdown finalizes the active clip with shutdown and ignores later input.
func (m *Machine) Shutdown() {
	if m.closed() {
		return
	}
	if m.active() {
		m.finish(ReasonShutdown, true, stateClosed)
	}
	m.cont = nil
	m.setState(stateClosed)
}

func (m *Machine) closed() bool { return m.state.Load() == stateClosed }

// codecChanged ends the active clip at the previous frame and, when still
// triggered, remembers to continue the series at the next IDR.
func (m *Machine) codecChanged() {
	if m.active() {
		series, next := m.cur.info.SeriesID, m.cur.info.SegmentIndex+1
		m.finish(ReasonCodecChange, true, stateIdle)
		if m.triggered && m.state.Load() == stateIdle {
			m.cont = &continuation{series: series, next: next}
		}
	}
	m.ring.Reset()
}

// tryOpen opens a clip from the ring when idle and triggered.
func (m *Machine) tryOpen() {
	if m.state.Load() != stateIdle || !m.triggered {
		return
	}
	gops := m.ring.Retained()
	if len(gops) == 0 {
		return // wait for the next IDR
	}
	var series ulid.ULID
	index := 0
	preRollMs := int64(0)
	if c := m.cont; c != nil {
		series, index = c.series, c.next
	} else {
		lastGOP := gops[len(gops)-1]
		newest := lastGOP.AUs[len(lastGOP.AUs)-1].Wall
		preRollMs = newest.Sub(gops[0].Start).Milliseconds()
	}
	switch m.begin(gops, series, index, preRollMs) {
	case beginOpened:
		m.cont = nil
		m.setState(stateRecording)
	case beginAborted:
		m.cont = nil // Suppressed was set; a new clip starts a new series
	case beginFailed:
		// Stay Idle; the next IDR retries.
	}
}

// beginResult says how begin ended.
type beginResult int

const (
	beginOpened  beginResult = iota // the segment is open and recording
	beginFailed                     // no segment was opened (ULID or Open error)
	beginAborted                    // opened, then ended by a write error: Suppressed
)

// begin opens a segment starting at the first of gops and writes every GOP
// but the last, which is still open and becomes pend. A zero series makes a
// new one. Errors are absorbed: the caller sees them as beginFailed (nothing
// opened) or beginAborted (the segment ended on a write error).
func (m *Machine) begin(gops []*ring.GOP, series ulid.ULID, index int, preRollMs int64) beginResult {
	start := gops[0].Start.UTC()
	id, err := ulid.New(ulid.Timestamp(start), m.rnd)
	if err != nil {
		return beginFailed
	}
	if series == (ulid.ULID{}) {
		series, err = ulid.New(ulid.Timestamp(start), m.rnd)
		if err != nil {
			return beginFailed
		}
	}
	info := OpenInfo{
		ClipID: id, SeriesID: series, SegmentIndex: index,
		Start: start, PreRollMs: preRollMs,
		Codec: m.codec, TimeSource: m.ts,
	}
	w, err := m.out.Open(info)
	if err != nil {
		return beginFailed
	}
	m.cur = &segment{w: w, info: info}
	last := len(gops) - 1
	for i, g := range gops[:last] {
		if !m.writeGOP(g, gops[i+1].Start) {
			return beginAborted
		}
	}
	m.cur.pend = append([]*camera.AccessUnit(nil), gops[last].AUs...)
	return beginOpened
}

// clipAU adds au to the active clip.
func (m *Machine) clipAU(au *camera.AccessUnit) {
	seg := m.cur
	if !au.IDR {
		seg.pend = append(seg.pend, au)
		return
	}
	// The IDR closes the pending GOP.
	if !m.flushPend(au.Wall) {
		return
	}
	if m.cfg.MaxClip > 0 && au.Wall.Sub(seg.info.Start) >= m.cfg.MaxClip {
		m.split(au)
		return
	}
	seg.pend = append(seg.pend[:0], au)
}

// split ends the segment with max_duration and opens the next at au, which
// is the IDR that starts it. The state is unchanged.
func (m *Machine) split(au *camera.AccessUnit) {
	series, next := m.cur.info.SeriesID, m.cur.info.SegmentIndex+1
	prev := m.state.Load()
	if !m.finish(ReasonMaxDuration, false, prev) {
		return
	}
	g := &ring.GOP{
		AUs: []*camera.AccessUnit{au}, Start: au.Wall, StartDTS: au.DTS,
		Bytes: int64(au.Bytes), ParamGen: au.ParamGen,
	}
	if m.begin([]*ring.GOP{g}, series, next, 0) != beginFailed {
		return
	}
	// Open failed. Retry at the next IDR, from GOPs that start at or after
	// this segment's end only: the older ring GOPs are already in the
	// previous segment.
	if m.triggered {
		m.cont = &continuation{series: series, next: next}
	}
	m.ring.Reset()
	m.ring.Push(au)
	m.setState(stateIdle)
}

// flushPend writes the pending GOP to the clip, ending at end. It reports
// false when the write failed and the clip was aborted.
func (m *Machine) flushPend(end time.Time) bool {
	seg := m.cur
	if len(seg.pend) == 0 {
		return true
	}
	first := seg.pend[0]
	var bytes int64
	for _, a := range seg.pend {
		bytes += int64(a.Bytes)
	}
	g := &ring.GOP{AUs: seg.pend, Start: first.Wall, StartDTS: first.DTS, Bytes: bytes, ParamGen: first.ParamGen}
	seg.pend = nil
	return m.writeGOP(g, end)
}

// writeGOP writes g; end is where it ends. On error the writer's backlog
// cannot take the clip: it ends with backlog_overflow and the machine is
// suppressed (relay-spec 4.5). The fragments already written are kept; a
// clip with none is aborted.
func (m *Machine) writeGOP(g *ring.GOP, end time.Time) bool {
	seg := m.cur
	if err := seg.w.WriteGOP(g); err != nil {
		m.finish(ReasonBacklogOverflow, false, stateSuppressed)
		return false
	}
	seg.fragments++
	seg.fragEnd = end
	return true
}

// finish ends the active clip with reason and moves to next. With flush the
// pending GOP is written first, ending one frame after its last unit. It
// reports false when a write failed (the machine is then Suppressed).
func (m *Machine) finish(reason EndReason, flush bool, next int32) bool {
	seg := m.cur
	if seg == nil {
		return false
	}
	if flush && len(seg.pend) > 0 {
		end := seg.pend[len(seg.pend)-1].Wall.Add(m.frameDur)
		if !m.flushPend(end) {
			return false
		}
	}
	m.cur = nil
	m.setState(next)
	if seg.fragments == 0 {
		// An empty plaintext is an error (Contract §4.3.4): discard.
		seg.w.Abort(reason)
		m.trig.endClip()
		return true
	}
	durMs := max(seg.fragEnd.Sub(seg.info.Start).Milliseconds(), 0)
	// A Finalize error is the writer's to handle (it owns the backlog and
	// the retries); the clip is over either way.
	_ = seg.w.Finalize(FinalInfo{
		End:        seg.fragEnd,
		DurationMs: durMs,
		Reason:     reason,
		Triggers:   m.trig.snapshot(seg.info.Start, durMs),
	})
	m.trig.endClip()
	return true
}
