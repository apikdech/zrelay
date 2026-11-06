package mux_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/mux"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const (
	h264Fixture = "../../../testdata/video/h264-640x360-gop2-10s.h264"
	h265Fixture = "../../../testdata/video/h265-640x360-gop2-5s.h265"
	fps         = 15
	tick        = 90000 / fps
)

// fixture loads an Annex-B file and returns its codec and GOPs, with DTS and
// Wall assigned the way a camera session would (DTS starts at 0). Parameter
// set NAL units move into the codec, as the RTSP session does.
func fixture(t *testing.T, path, kind string) (camera.Codec, []*ring.GOP) {
	t.Helper()
	aus := testutil.LoadAnnexB(t, path)
	c := camera.Codec{Kind: kind, Width: 640, Height: 360}
	wall := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var gops []*ring.GOP
	for i, au := range aus {
		var nalus [][]byte
		var size int
		for _, n := range au.NALUs {
			typ := n[0] & 0x1f
			if kind == "h265" {
				typ = (n[0] >> 1) & 0x3f
			}
			switch {
			case kind == "h264" && typ == 7:
				c.SPS = n
			case kind == "h264" && typ == 8:
				c.PPS = n
			case kind == "h265" && typ == 32:
				c.VPS = n
			case kind == "h265" && typ == 33:
				c.SPS = n
			case kind == "h265" && typ == 34:
				c.PPS = n
			default:
				nalus = append(nalus, n)
				size += len(n)
			}
		}
		a := &camera.AccessUnit{
			PTS: int64(i) * tick, DTS: int64(i) * tick, IDR: au.IDR, NALUs: nalus,
			Wall: wall.Add(time.Duration(i) * time.Second / fps), Bytes: size,
		}
		if au.IDR {
			gops = append(gops, &ring.GOP{Start: a.Wall, StartDTS: a.DTS})
		}
		g := gops[len(gops)-1]
		g.AUs = append(g.AUs, a)
		g.Bytes += int64(size)
	}
	return c, gops
}

type box struct {
	typ        string
	start, end int
}

func topBoxes(t *testing.T, b []byte) []box {
	t.Helper()
	var out []box
	for pos := 0; pos < len(b); {
		if pos+8 > len(b) {
			t.Fatalf("truncated box header at %d", pos)
		}
		size := int(binary.BigEndian.Uint32(b[pos:]))
		if size < 8 || pos+size > len(b) {
			t.Fatalf("bad box size %d at %d", size, pos)
		}
		out = append(out, box{string(b[pos+4 : pos+8]), pos, pos + size})
		pos += size
	}
	return out
}

func muxAll(t *testing.T, path, kind string) (*mux.Muxer, *bytes.Buffer, []*ring.GOP) {
	t.Helper()
	c, gops := fixture(t, path, kind)
	var buf bytes.Buffer
	m, err := mux.New(&buf, c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, g := range gops {
		if err := m.WriteGOP(g); err != nil {
			t.Fatalf("WriteGOP: %v", err)
		}
	}
	return m, &buf, gops
}

func TestCodecStringH264(t *testing.T) {
	got, err := mux.CodecString(camera.Codec{Kind: "h264", SPS: []byte{0x67, 0x64, 0x00, 0x32, 0xac}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "avc1.640032" {
		t.Fatalf("got %q, want avc1.640032", got)
	}
}

func TestCodecStringErrors(t *testing.T) {
	for name, c := range map[string]camera.Codec{
		"h264 short sps": {Kind: "h264", SPS: []byte{0x67, 0x64}},
		"h265 short sps": {Kind: "h265", SPS: []byte{0x42, 0x01, 0x01, 0x01}},
		"unknown kind":   {Kind: "vp9", SPS: []byte{0x67, 0x64, 0, 0x32}},
	} {
		if _, err := mux.CodecString(c); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestCodecStringH265(t *testing.T) {
	// NAL header 42 01, vps_id/sub_layers/nesting 01, then profile_tier_level:
	// Main (profile 1, tier 0), compat flags 60 00 00 00, constraint B0 00.., level 90.
	// Zero runs are emulation-prevented (00 00 03).
	sps := []byte{0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00, 0xB0, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x5A, 0xA0}
	got, err := mux.CodecString(camera.Codec{Kind: "h265", SPS: sps})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hvc1.1.6.L90.B0" {
		t.Fatalf("got %q, want hvc1.1.6.L90.B0", got)
	}
	// High tier, profile 2 (compat flags 20 00 00 00, written bit-reversed
	// as hex 4), constraint byte 90, level 93.
	sps = []byte{0x42, 0x01, 0x01, 0x22, 0x20, 0x00, 0x00, 0x03, 0x00, 0x90, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x5D, 0xA0}
	got, err = mux.CodecString(camera.Codec{Kind: "h265", SPS: sps})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hvc1.2.4.H93.90" {
		t.Fatalf("got %q, want hvc1.2.4.H93.90", got)
	}
}

func TestCodecStringH265FromFixture(t *testing.T) {
	c, _ := fixture(t, h265Fixture, "h265")
	got, err := mux.CodecString(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 10 || got[:10] != "hvc1.1.6.L" {
		t.Fatalf("got %q, want hvc1.1.6.L<level>... for an x265 Main stream", got)
	}
}

func TestMuxedOutputProbes(t *testing.T) {
	_, buf, gops := muxAll(t, h264Fixture, "h264")

	out := testutil.FFProbe(t, buf.Bytes(), "-select_streams", "v:0", "-show_frames", "-show_streams", "-of", "json")
	var res struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Frames []struct {
			KeyFrame int `json:"key_frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("ffprobe json: %v", err)
	}
	if len(res.Streams) != 1 || res.Streams[0].CodecName != "h264" || res.Streams[0].Width != 640 || res.Streams[0].Height != 360 {
		t.Fatalf("streams = %+v", res.Streams)
	}
	if len(res.Frames) != 150 {
		t.Fatalf("frames = %d, want 150", len(res.Frames))
	}
	idx := 0
	for gi, g := range gops {
		for i := range g.AUs {
			want := 0
			if i == 0 {
				want = 1
			}
			if res.Frames[idx].KeyFrame != want {
				t.Fatalf("gop %d frame %d key_frame=%d, want %d", gi, i, res.Frames[idx].KeyFrame, want)
			}
			idx++
		}
	}
}

func TestSeekIndexPointsAtMoof(t *testing.T) {
	m, buf, gops := muxAll(t, h264Fixture, "h264")
	st := m.Stats()
	if len(st.SeekIndex) != len(gops) {
		t.Fatalf("seek index has %d entries, want %d", len(st.SeekIndex), len(gops))
	}
	b := buf.Bytes()
	for i, e := range st.SeekIndex {
		off := e[1]
		if string(b[off+4:off+8]) != "moof" {
			t.Fatalf("entry %d offset %d: bytes %q, want moof", i, off, b[off+4:off+8])
		}
		if want := int64(i) * 2000; e[0] != want {
			t.Errorf("entry %d t_ms=%d, want %d", i, e[0], want)
		}
	}
	if st.DurationMs != 10000 {
		t.Errorf("DurationMs = %d, want 10000", st.DurationMs)
	}
	if st.PlaintextBytes != int64(buf.Len()) {
		t.Errorf("PlaintextBytes = %d, want %d", st.PlaintextBytes, buf.Len())
	}
}

func TestInitBytesMatchesFtypMoov(t *testing.T) {
	m, buf, _ := muxAll(t, h264Fixture, "h264")
	boxes := topBoxes(t, buf.Bytes())
	if len(boxes) < 4 || boxes[0].typ != "ftyp" || boxes[1].typ != "moov" {
		t.Fatalf("boxes = %+v", boxes)
	}
	if got, want := m.Stats().InitSegmentBytes, int64(boxes[1].end); got != want {
		t.Fatalf("InitSegmentBytes = %d, want %d", got, want)
	}
	for i, bx := range boxes[2:] {
		want := "moof"
		if i%2 == 1 {
			want = "mdat"
		}
		if bx.typ != want {
			t.Fatalf("box %d is %s, want %s", i+2, bx.typ, want)
		}
	}
}

func TestNewWritesInitImmediately(t *testing.T) {
	c, _ := fixture(t, h264Fixture, "h264")
	var buf bytes.Buffer
	m, err := mux.New(&buf, c)
	if err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 || int64(buf.Len()) != m.Stats().InitSegmentBytes {
		t.Fatalf("buffer %d bytes, InitSegmentBytes %d", buf.Len(), m.Stats().InitSegmentBytes)
	}
}

// tfdt is relative to the first GOP's StartDTS, even when that is large (an
// unwrapped DTS far past a 32-bit RTP wrap).
func TestTfdtRelativeToFirstGOP(t *testing.T) {
	c, gops := fixture(t, h264Fixture, "h264")
	const base = int64(1 << 33)
	for _, g := range gops {
		g.StartDTS += base
		for _, a := range g.AUs {
			a.DTS += base
			a.PTS += base
		}
	}
	var buf bytes.Buffer
	m, err := mux.New(&buf, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gops {
		if err := m.WriteGOP(g); err != nil {
			t.Fatal(err)
		}
	}
	b := buf.Bytes()
	idx := m.Stats().SeekIndex
	for i, e := range idx {
		end := len(b)
		if i+1 < len(idx) {
			end = int(idx[i+1][1])
		}
		pos := bytes.Index(b[e[1]:end], []byte("tfdt"))
		if pos < 0 {
			t.Fatalf("fragment %d has no tfdt", i)
		}
		// tfdt payload: version/flags (4 bytes), then the 64-bit base time (version 1).
		got := binary.BigEndian.Uint64(b[int(e[1])+pos+8:])
		if want := uint64(i) * 30 * tick; got != want {
			t.Fatalf("fragment %d tfdt = %d, want %d", i, got, want)
		}
	}
}

func TestH265UsesHvc1(t *testing.T) {
	m, buf, gops := muxAll(t, h265Fixture, "h265")
	b := buf.Bytes()
	if !bytes.Contains(b, []byte("hvc1")) {
		t.Fatal("init segment has no hvc1 sample entry")
	}
	if bytes.Contains(b, []byte("hev1")) {
		t.Fatal("init segment must not use hev1")
	}
	if len(m.Stats().SeekIndex) != len(gops) {
		t.Fatalf("seek index entries = %d, want %d", len(m.Stats().SeekIndex), len(gops))
	}
	out := testutil.FFProbe(t, b, "-select_streams", "v:0", "-show_frames", "-show_streams", "-of", "json")
	var res struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecTag  string `json:"codec_tag_string"`
		} `json:"streams"`
		Frames []struct{} `json:"frames"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Streams) != 1 || res.Streams[0].CodecName != "hevc" || res.Streams[0].CodecTag != "hvc1" || len(res.Frames) != 75 {
		t.Fatalf("ffprobe: %+v frames=%d", res.Streams, len(res.Frames))
	}
}

func TestWriteGOPRejectsEmpty(t *testing.T) {
	c, _ := fixture(t, h264Fixture, "h264")
	m, err := mux.New(&bytes.Buffer{}, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteGOP(&ring.GOP{}); err == nil {
		t.Fatal("want error for an empty GOP")
	}
}

var errWrite = errors.New("write failed")

// limitWriter accepts the first n bytes then fails.
type limitWriter struct{ n int }

func (w *limitWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		return 0, errWrite
	}
	w.n -= len(p)
	return len(p), nil
}

func TestWriteErrorsPropagate(t *testing.T) {
	c, gops := fixture(t, h264Fixture, "h264")
	if _, err := mux.New(&limitWriter{}, c); !errors.Is(err, errWrite) {
		t.Fatalf("New error = %v, want write error", err)
	}
	var probe bytes.Buffer
	if _, err := mux.New(&probe, c); err != nil {
		t.Fatal(err)
	}
	m, err := mux.New(&limitWriter{n: probe.Len()}, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteGOP(gops[0]); !errors.Is(err, errWrite) {
		t.Fatalf("WriteGOP error = %v, want write error", err)
	}
}
