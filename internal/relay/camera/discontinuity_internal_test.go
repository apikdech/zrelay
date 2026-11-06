package camera

import (
	"fmt"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"

	"github.com/apikdech/zrelay-v2/internal/testutil"
)

// runWithTimestampJump feeds total access units (the 150-AU fixture, looped,
// so an IDR comes every 30 AUs) through the RTP packetizer and the RunRTSP
// video path in arrival mode. From AU jumpAt on, the camera's RTP timestamp
// is shifted by jump ticks (modulo 2^32), as after an encoder or RTP clock
// restart; packets keep arriving at the real frame rate.
func runWithTimestampJump(t *testing.T, jump uint32, jumpAt, total int) (*memSink, *assembler, time.Time) {
	t.Helper()
	return runJump(t, jump, jumpAt, total, -1)
}

// runJump is runWithTimestampJump with an optional rtcp time source. With
// srAt >= 0 the camera sends RTCP sender reports: one at the session start
// (arrival of AU 0 <-> its RTP timestamp) and, srAt AUs after the jump, a
// correct one on the clock in force then. Between the jump and that report
// the mapping is stale, as gortsplib's is (it extrapolates the last report
// with an int32 timestamp difference). With srAt < 0 there are none (arrival
// mode).
func runJump(t *testing.T, jump uint32, jumpAt, total, srAt int) (*memSink, *assembler, time.Time) {
	t.Helper()
	aus := testutil.LoadAnnexB(t, h264FixturePath)
	enc := &rtph264.Encoder{PayloadType: 96, PayloadMaxSize: 1450, PacketizationMode: 1}
	if err := enc.Init(); err != nil {
		t.Fatal(err)
	}
	dec := &rtph264.Decoder{PacketizationMode: 1}
	if err := dec.Init(); err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	const start = uint32(0x1000_0000)
	arrival := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cur := 0
	arrivalOf := func(i int) time.Time { return arrival.Add(ticksToDuration(int64(i * auTicks))) }
	tsOf := func(i int) uint32 {
		ts := start + uint32(i*auTicks)
		if i >= jumpAt {
			ts += jump
		}
		return ts
	}
	ntp := noNTP
	if srAt >= 0 {
		ntp = func(rtp uint32) (time.Time, bool) {
			base, baseTS := arrivalOf(0), start
			if cur >= jumpAt+srAt {
				base, baseTS = arrivalOf(jumpAt+srAt), tsOf(jumpAt+srAt)
			}
			return base.Add(ticksToDuration(int64(int32(rtp - baseTS)))), true
		}
	}
	a := newAssembler(h264Rules, nil, nil, nil, sink, ntp)
	v := newVideoPath(a, dec)
	var last time.Time
	for i := range total {
		pkts, err := enc.Encode(aus[i%len(aus)].NALUs)
		if err != nil {
			t.Fatal(err)
		}
		cur = i
		ts := tsOf(i)
		last = arrivalOf(i)
		for _, p := range pkts {
			p.Timestamp = ts
			v.packet(p, last)
		}
	}
	return sink, a, last
}

// TestTimestampJumpResumesWithinOneGOP: a camera that restarts its RTP clock
// mid-session must not silence the stream. Delivery resumes within one GOP of
// the jump, DTS and Wall stay strictly increasing, no sample is longer than a
// couple of seconds, and Wall follows the arrival time rather than the jump.
func TestTimestampJumpResumesWithinOneGOP(t *testing.T) {
	t.Parallel()
	const (
		gop   = 30
		total = 600
	)
	neg := func(sec int) uint32 { return -uint32(sec * videoClockRate) }
	for _, tc := range []struct {
		name   string
		jump   uint32
		jumpAt int
	}{
		{"back 10 min at IDR", neg(600), 300},
		{"back 10 min mid-GOP", neg(600), 315},
		{"back 1 s at IDR", neg(1), 300},
		{"back 1 s mid-GOP", neg(1), 315},
		{"forward 2^31+ at IDR", 0x9000_0000, 300},
		{"forward 2^31+ mid-GOP", 0x9000_0000, 315},
		{"forward under 2^31 at IDR", 0x4000_0000, 300},
		{"forward under 2^31 mid-GOP", 0x4000_0000, 315},
		{"forward 1 h at IDR", uint32(3600 * videoClockRate), 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink, a, lastArrival := runWithTimestampJump(t, tc.jump, tc.jumpAt, total)
			got := sink.videos()
			if len(got) < total-gop-2 {
				t.Fatalf("delivered %d of %d AUs, want at least %d", len(got), total, total-gop-2)
			}
			if a.discontinuities == 0 {
				t.Fatal("the jump was not counted as a discontinuity")
			}
			for i := 1; i < len(got); i++ {
				if got[i].DTS <= got[i-1].DTS {
					t.Fatalf("AU %d: DTS %d not after %d", i, got[i].DTS, got[i-1].DTS)
				}
				if step := got[i].DTS - got[i-1].DTS; step > 2*videoClockRate {
					t.Fatalf("AU %d: DTS step %d ticks (%v)", i, step, ticksToDuration(step))
				}
				if !got[i].Wall.After(got[i-1].Wall) {
					t.Fatalf("AU %d: Wall %v not after %v", i, got[i].Wall, got[i-1].Wall)
				}
				if d := got[i].Wall.Sub(got[i-1].Wall); d > 2*time.Second {
					t.Fatalf("AU %d: Wall step %v", i, d)
				}
			}
			final := got[len(got)-1]
			if d := final.Wall.Sub(lastArrival); d < -time.Second || d > time.Second {
				t.Fatalf("last Wall %v is %v from the last arrival %v", final.Wall, d, lastArrival)
			}
			// Delivery is continuous after the resume: the last 100 AUs
			// are all present.
			if n := len(got); got[n-1].DTS-got[n-100].DTS != 99*auTicks {
				t.Fatalf("tail is not continuous: %d ticks over 99 AUs", got[n-1].DTS-got[n-100].DTS)
			}
		})
	}
}

// TestTimestampJumpKeepsWallNearArrivalInRTCPMode: with a camera that sends
// sender reports, a jump leaves the NTP mapping stale until the next report
// on the new clock. Wall must not follow the stale mapping (an hour or more
// ahead) and then ratchet there: it never runs ahead of the real time and
// ends near the last arrival.
func TestTimestampJumpKeepsWallNearArrivalInRTCPMode(t *testing.T) {
	t.Parallel()
	neg := func(sec int) uint32 { return -uint32(sec * videoClockRate) }
	for _, tc := range []struct {
		name string
		jump uint32
	}{
		{"forward 1 h", uint32(3600 * videoClockRate)},
		{"forward 0x4000_0000", 0x4000_0000},
		{"forward 2^31+", 0x9000_0000},
		{"back 10 min", neg(600)},
		{"back 1 s", neg(1)},
	} {
		for _, jumpAt := range []int{300, 315} {
			t.Run(fmt.Sprintf("%s at %d", tc.name, jumpAt), func(t *testing.T) {
				t.Parallel()
				const total = 900
				sink, a, lastArrival := runJump(t, tc.jump, jumpAt, total, 150)
				if a.timeSource != TimeSourceRTCP {
					t.Fatalf("time source %q, want rtcp", a.timeSource)
				}
				if a.discontinuities == 0 {
					t.Fatal("the jump was not counted as a discontinuity")
				}
				got := sink.videos()
				if len(got) < total-32 {
					t.Fatalf("delivered %d of %d AUs", len(got), total)
				}
				for i, au := range got {
					if i > 0 && !au.Wall.After(got[i-1].Wall) {
						t.Fatalf("AU %d: Wall %v not after %v", i, au.Wall, got[i-1].Wall)
					}
					if au.Wall.After(lastArrival.Add(time.Second)) {
						t.Fatalf("AU %d: Wall %v is ahead of the last arrival %v", i, au.Wall, lastArrival)
					}
				}
				final := got[len(got)-1]
				if d := final.Wall.Sub(lastArrival); d < -time.Second || d > time.Second {
					t.Fatalf("last Wall %v is %v from the last arrival %v", final.Wall, d, lastArrival)
				}
			})
		}
	}
}

// TestSmallTimestampStepsAreNotDiscontinuities: ordinary jitter and a short
// stall must not trigger a re-base.
func TestSmallTimestampStepsAreNotDiscontinuities(t *testing.T) {
	t.Parallel()
	// A 5 s forward step (a camera that stalled) stays within bounds.
	sink, a, _ := runWithTimestampJump(t, 5*videoClockRate, 300, 450)
	if a.discontinuities != 0 {
		t.Fatalf("%d discontinuities for a 5 s step", a.discontinuities)
	}
	if got := len(sink.videos()); got != 450 {
		t.Fatalf("delivered %d AUs, want 450", got)
	}
}
