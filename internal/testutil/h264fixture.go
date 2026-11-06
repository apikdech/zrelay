package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AU is one access unit of an elementary stream: its NAL units in decode
// order, without start codes, and whether it is an IDR (random access) picture.
type AU struct {
	NALUs [][]byte
	IDR   bool
}

// LoadAnnexB reads an Annex-B elementary stream and splits it into access
// units. The codec is chosen by extension: .h265, .hevc and .265 are H.265,
// anything else is H.264. Any read or parse problem fails the test.
func LoadAnnexB(t testing.TB, path string) []AU {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testutil.LoadAnnexB: %v", err)
	}
	c := h264Rules
	switch strings.ToLower(filepath.Ext(path)) {
	case ".h265", ".hevc", ".265":
		c = h265Rules
	}
	aus := splitAccessUnits(splitAnnexB(data), c)
	if len(aus) == 0 {
		t.Fatalf("testutil.LoadAnnexB: %s holds no access units", path)
	}
	return aus
}

// splitAnnexB returns the NAL units of an Annex-B byte stream, without start
// codes or trailing zero bytes.
func splitAnnexB(b []byte) [][]byte {
	var nalus [][]byte
	start := -1
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				nalus = appendNALU(nalus, b[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 {
		nalus = appendNALU(nalus, b[start:])
	}
	return nalus
}

func appendNALU(nalus [][]byte, n []byte) [][]byte {
	// Zero bytes before a start code (a 4-byte start code's leading zero,
	// or trailing_zero_8bits) are not part of the NAL unit.
	for len(n) > 0 && n[len(n)-1] == 0 {
		n = n[:len(n)-1]
	}
	if len(n) == 0 {
		return nalus
	}
	return append(nalus, append([]byte(nil), n...))
}

// codecRules classifies NAL units for access-unit splitting.
type codecRules struct {
	// vcl reports whether the NAL unit carries slice data.
	vcl func(n []byte) bool
	// firstSlice reports whether a VCL NAL unit is the first slice of a picture.
	firstSlice func(n []byte) bool
	// opensAU reports whether a non-VCL NAL unit starts a new access unit
	// when it follows a VCL NAL unit.
	opensAU func(n []byte) bool
	// idr reports whether a NAL unit is an IDR slice.
	idr func(n []byte) bool
}

var h264Rules = codecRules{
	vcl:        func(n []byte) bool { t := n[0] & 0x1f; return t >= 1 && t <= 5 },
	firstSlice: func(n []byte) bool { return len(n) > 1 && n[1]&0x80 != 0 }, // first_mb_in_slice == 0
	opensAU: func(n []byte) bool {
		t := n[0] & 0x1f
		return (t >= 6 && t <= 9) || (t >= 14 && t <= 18)
	},
	idr: func(n []byte) bool { return n[0]&0x1f == 5 },
}

func h265NALType(n []byte) byte { return (n[0] >> 1) & 0x3f }

var h265Rules = codecRules{
	vcl:        func(n []byte) bool { return h265NALType(n) <= 31 },
	firstSlice: func(n []byte) bool { return len(n) > 2 && n[2]&0x80 != 0 }, // first_slice_segment_in_pic_flag
	opensAU: func(n []byte) bool {
		t := h265NALType(n)
		return (t >= 32 && t <= 35) || t == 39 || (t >= 41 && t <= 44) || (t >= 48 && t <= 55)
	},
	idr: func(n []byte) bool { t := h265NALType(n); return t == 19 || t == 20 },
}

// splitAccessUnits groups NAL units into access units (H.264 §7.4.1.2.3,
// H.265 §7.4.2.4.4): a new unit starts at an opening non-VCL NAL unit or at
// the first slice of a picture, once the current unit holds a slice.
func splitAccessUnits(nalus [][]byte, c codecRules) []AU {
	var aus []AU
	var cur AU
	hasVCL := false
	flush := func() {
		if len(cur.NALUs) > 0 {
			aus = append(aus, cur)
		}
		cur, hasVCL = AU{}, false
	}
	for _, n := range nalus {
		isVCL := c.vcl(n)
		if hasVCL && ((isVCL && c.firstSlice(n)) || (!isVCL && c.opensAU(n))) {
			flush()
		}
		cur.NALUs = append(cur.NALUs, n)
		if isVCL {
			hasVCL = true
			if c.idr(n) {
				cur.IDR = true
			}
		}
	}
	flush()
	return aus
}
