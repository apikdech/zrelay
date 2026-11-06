package camera

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/pion/rtp"

	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const (
	h264FixturePath = "../../../testdata/video/h264-640x360-gop2-10s.h264"
	h265FixturePath = "../../../testdata/video/h265-640x360-gop2-5s.h265"
	auTicks         = 90000 / 15
)

// sinkEvent is one SessionSink call, in order.
type sinkEvent struct {
	codec *Codec
	ts    TimeSource
	video *AccessUnit
}

type memSink struct{ events []sinkEvent }

func (s *memSink) Codec(c Codec, ts TimeSource) {
	s.events = append(s.events, sinkEvent{codec: &c, ts: ts})
}
func (s *memSink) Video(au *AccessUnit) { s.events = append(s.events, sinkEvent{video: au}) }
func (s *memSink) Audio(*AudioPacket)   {}

func (s *memSink) videos() []*AccessUnit {
	var out []*AccessUnit
	for _, e := range s.events {
		if e.video != nil {
			out = append(out, e.video)
		}
	}
	return out
}

func (s *memSink) codecs() []Codec {
	var out []Codec
	for _, e := range s.events {
		if e.codec != nil {
			out = append(out, *e.codec)
		}
	}
	return out
}

func noNTP(uint32) (time.Time, bool) { return time.Time{}, false }

type rtpEncoder interface {
	Encode([][]byte) ([]*rtp.Packet, error)
}

// TestVideoPathStampsAccessUnits feeds the fixtures through the RTP
// packetizer and the same depacketize-and-assemble path RunRTSP uses, with
// access units that fit in one RTP packet, span several, or a mix of both,
// and with a camera that never sets the RTP marker bit.
func TestVideoPathStampsAccessUnits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		codec      string
		payloadMax int
		noMarker   bool
	}{
		{codec: "h264", payloadMax: 1 << 20},
		{codec: "h264", payloadMax: 4000},
		{codec: "h264", payloadMax: 1450},
		{codec: "h264", payloadMax: 4000, noMarker: true},
		{codec: "h264", payloadMax: 1 << 20, noMarker: true},
		{codec: "h265", payloadMax: 1 << 20},
		{codec: "h265", payloadMax: 4000},
		{codec: "h265", payloadMax: 1450},
	} {
		name := fmt.Sprintf("%s/payload%d/nomarker=%v", tc.codec, tc.payloadMax, tc.noMarker)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var (
				aus   []testutil.AU
				rules codecRules
				enc   rtpEncoder
				dec   decoder
			)
			switch tc.codec {
			case "h264":
				aus = testutil.LoadAnnexB(t, h264FixturePath)
				rules = h264Rules
				e := &rtph264.Encoder{PayloadType: 96, PayloadMaxSize: tc.payloadMax, PacketizationMode: 1}
				if err := e.Init(); err != nil {
					t.Fatal(err)
				}
				d := &rtph264.Decoder{PacketizationMode: 1}
				if err := d.Init(); err != nil {
					t.Fatal(err)
				}
				enc, dec = e, d
			case "h265":
				aus = testutil.LoadAnnexB(t, h265FixturePath)
				rules = h265Rules
				e := &rtph265.Encoder{PayloadType: 96, PayloadMaxSize: tc.payloadMax}
				if err := e.Init(); err != nil {
					t.Fatal(err)
				}
				d := &rtph265.Decoder{}
				if err := d.Init(); err != nil {
					t.Fatal(err)
				}
				enc, dec = e, d
			}

			sink := &memSink{}
			v := newVideoPath(newAssembler(rules, nil, nil, nil, sink, noNTP), dec)
			const start = uint32(0xFFFF_0000) // crosses the RTP wrap too
			arrival := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			single := 0
			for i, au := range aus {
				pkts, err := enc.Encode(au.NALUs)
				if err != nil {
					t.Fatalf("encode AU %d: %v", i, err)
				}
				if len(pkts) == 1 {
					single++
				}
				for _, p := range pkts {
					p.Timestamp = start + uint32(i*auTicks)
					if tc.noMarker {
						p.Marker = false
					}
					v.packet(p, arrival.Add(ticksToDuration(int64(i*auTicks))))
				}
			}
			t.Logf("%d of %d AUs in one RTP packet", single, len(aus))

			want := len(aus)
			if tc.noMarker {
				// Without markers an AU completes when the next one starts,
				// so the last one stays in the depacketizer.
				want--
			}
			got := sink.videos()
			if len(got) != want {
				t.Fatalf("delivered %d AUs, want %d", len(got), want)
			}
			for i, au := range got {
				if au.PTS != int64(i*auTicks) || au.DTS != int64(i*auTicks) {
					t.Fatalf("AU %d: PTS/DTS %d/%d, want %d", i, au.PTS, au.DTS, i*auTicks)
				}
				last := au.NALUs[len(au.NALUs)-1]
				fix := aus[i].NALUs[len(aus[i].NALUs)-1]
				if !bytes.Equal(last, fix) {
					t.Fatalf("AU %d is not fixture AU %d", i, i)
				}
				if au.IDR != aus[i].IDR {
					t.Fatalf("AU %d IDR %v, fixture %v", i, au.IDR, aus[i].IDR)
				}
				if wantWall := arrival.Add(ticksToDuration(au.DTS)); !au.Wall.Equal(wantWall) {
					t.Fatalf("AU %d Wall %v, want %v", i, au.Wall, wantWall)
				}
			}
		})
	}
}

// stripParamSets returns the fixture AUs without in-band SPS/PPS.
func stripParamSets(aus []testutil.AU) [][][]byte {
	out := make([][][]byte, len(aus))
	for i, au := range aus {
		for _, n := range au.NALUs {
			if h264Rules.paramSet(n) == 0 {
				out[i] = append(out[i], n)
			}
		}
	}
	return out
}

func firstParamSet(aus []testutil.AU, kind byte) []byte {
	for _, au := range aus {
		for _, n := range au.NALUs {
			if h264Rules.paramSet(n) == kind {
				return n
			}
		}
	}
	return nil
}

// TestCodecAnnouncedWhenChangingAUIsDropped: a parameter-set change carried
// by an access unit that is then dropped must still be announced by a Codec
// call before the first access unit of the new generation.
func TestCodecAnnouncedWhenChangingAUIsDropped(t *testing.T) {
	t.Parallel()
	fix := testutil.LoadAnnexB(t, h264FixturePath)
	nalus := stripParamSets(fix)
	sps, pps := firstParamSet(fix, 's'), firstParamSet(fix, 'p')
	newSPS := bytes.Clone(sps)
	newSPS[3]++ // level_idc: a different SPS that parses the same way

	with := func(ps []byte, au [][]byte) [][]byte { return append([][]byte{ps}, au...) }
	type step struct {
		nalus [][]byte
		ticks int64
	}
	for _, tc := range []struct {
		name  string
		steps func() []step
	}{
		{
			// A non-monotonic DTS on AU 5 makes the session wait for the
			// next IDR; AU 6 brings the new SPS and is dropped.
			name: "non-IDR while waiting for IDR",
			steps: func() []step {
				var s []step
				for i := range 5 {
					s = append(s, step{nalus[i], int64(i * auTicks)})
				}
				s = append(s, step{nalus[5], 4 * auTicks})
				s = append(s, step{with(newSPS, nalus[6]), 6 * auTicks})
				for i := 7; i < 35; i++ {
					s = append(s, step{nalus[i], int64(i * auTicks)})
				}
				return s
			},
		},
		{
			// The IDR carrying the new SPS has a non-increasing DTS.
			name: "IDR with non-increasing DTS",
			steps: func() []step {
				var s []step
				for i := range 5 {
					s = append(s, step{nalus[i], int64(i * auTicks)})
				}
				s = append(s, step{with(newSPS, nalus[30]), 4 * auTicks})
				for i := 31; i < 65; i++ {
					s = append(s, step{nalus[i], int64(i * auTicks)})
				}
				return s
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := &memSink{}
			wall := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			ntp := func(rtp uint32) (time.Time, bool) { return wall.Add(ticksToDuration(int64(rtp))), true }
			a := newAssembler(h264Rules, nil, sps, pps, sink, ntp)
			for _, s := range tc.steps() {
				a.accessUnit(s.nalus, uint32(s.ticks), s.ticks)
			}

			gen1 := 0
			calls := 0
			for _, e := range sink.events {
				if e.codec != nil {
					calls++
					continue
				}
				if want := uint32(calls - 1); e.video.ParamGen != want {
					t.Fatalf("AU DTS %d: ParamGen %d after %d Codec calls, want %d", e.video.DTS, e.video.ParamGen, calls, want)
				}
				if e.video.ParamGen == 1 {
					gen1++
				}
			}
			if gen1 == 0 {
				t.Fatal("no AU with ParamGen 1 was delivered")
			}
			cs := sink.codecs()
			if len(cs) != 2 || !bytes.Equal(cs[1].SPS, newSPS) {
				t.Fatalf("Codec calls %d, want 2 with the new SPS last", len(cs))
			}
		})
	}
}

// TestWallClockRTCPNeverStepsBack: in rtcp mode Wall follows the sender
// report mapping, except that it never runs slower than the media clock: a
// backward step of the mapping is absorbed (Wall keeps advancing by the DTS
// step), a forward step is followed.
func TestWallClockRTCPNeverStepsBack(t *testing.T) {
	t.Parallel()
	fix := testutil.LoadAnnexB(t, h264FixturePath)
	nalus := stripParamSets(fix)
	sink := &memSink{}
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var offset time.Duration
	ntp := func(rtp uint32) (time.Time, bool) {
		return base.Add(ticksToDuration(int64(rtp)) + offset), true
	}
	a := newAssembler(h264Rules, nil, firstParamSet(fix, 's'), firstParamSet(fix, 'p'), sink, ntp)
	for i := range 45 {
		switch i {
		case 10:
			offset = -time.Second // the camera's NTP steps back
		case 20:
			offset = 5 * time.Second // and then forward
		}
		ticks := int64(i * auTicks)
		a.accessUnit(nalus[i], uint32(ticks), ticks)
	}
	got := sink.videos()
	if len(got) != 45 {
		t.Fatalf("delivered %d AUs, want 45", len(got))
	}
	step := ticksToDuration(auTicks)
	for i := 1; i < len(got); i++ {
		if !got[i].Wall.After(got[i-1].Wall) {
			t.Fatalf("AU %d: Wall %v not after %v", i, got[i].Wall, got[i-1].Wall)
		}
	}
	for i := 10; i < 20; i++ {
		if d := got[i].Wall.Sub(got[i-1].Wall); d != step {
			t.Fatalf("AU %d after the backward step: Wall step %v, want %v", i, d, step)
		}
	}
	if want := base.Add(ticksToDuration(got[20].DTS) + 5*time.Second); !got[20].Wall.Equal(want) {
		t.Fatalf("AU 20 after the forward step: Wall %v, want %v", got[20].Wall, want)
	}
}
