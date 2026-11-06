package testutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apikdech/zrelay-v2/internal/testutil"
)

const (
	h264Fixture = "../../testdata/video/h264-640x360-gop2-10s.h264"
	h265Fixture = "../../testdata/video/h265-640x360-gop2-5s.h265"
)

func h264Type(nalu []byte) byte { return nalu[0] & 0x1f }
func h265Type(nalu []byte) byte { return (nalu[0] >> 1) & 0x3f }

func TestLoadAnnexBFindsIDREveryTwoSeconds(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h264Fixture)
	if len(aus) != 150 {
		t.Fatalf("got %d access units, want 150", len(aus))
	}
	for i, au := range aus {
		want := i%30 == 0
		if au.IDR != want {
			t.Errorf("AU %d: IDR=%v, want %v", i, au.IDR, want)
		}
		if len(au.NALUs) == 0 {
			t.Fatalf("AU %d has no NALUs", i)
		}
		hasIDRSlice := false
		for _, n := range au.NALUs {
			if len(n) == 0 {
				t.Fatalf("AU %d has an empty NALU", i)
			}
			if n[0] == 0 && len(n) > 2 && n[1] == 0 && (n[2] == 1 || n[2] == 0) {
				t.Fatalf("AU %d NALU still carries a start code", i)
			}
			if h264Type(n) == 5 {
				hasIDRSlice = true
			}
		}
		if hasIDRSlice != want {
			t.Errorf("AU %d: IDR slice present=%v, want %v", i, hasIDRSlice, want)
		}
		if want {
			types := map[byte]bool{}
			for _, n := range au.NALUs {
				types[h264Type(n)] = true
			}
			if !types[7] || !types[8] {
				t.Errorf("IDR AU %d lacks in-band SPS/PPS (types %v)", i, types)
			}
		}
	}
}

func TestLoadAnnexBH265FindsIDREveryTwoSeconds(t *testing.T) {
	aus := testutil.LoadAnnexB(t, h265Fixture)
	if len(aus) != 75 {
		t.Fatalf("got %d access units, want 75", len(aus))
	}
	for i, au := range aus {
		want := i%30 == 0
		if au.IDR != want {
			t.Errorf("AU %d: IDR=%v, want %v", i, au.IDR, want)
		}
		if want {
			types := map[byte]bool{}
			for _, n := range au.NALUs {
				types[h265Type(n)] = true
			}
			if !types[32] || !types[33] || !types[34] {
				t.Errorf("IDR AU %d lacks in-band VPS/SPS/PPS (types %v)", i, types)
			}
		}
	}
}

func TestLoadAnnexBSplitsOnFirstSliceWithoutAUD(t *testing.T) {
	// Two P frames, each a single slice with first_mb_in_slice == 0
	// (first slice-header bit set), and an SEI that opens the second AU.
	stream := []byte{
		0, 0, 0, 1, 0x41, 0x80, 0x11, // non-IDR slice, first_mb=0
		0, 0, 1, 0x41, 0x40, 0x22, // non-IDR slice, first_mb!=0 (same picture)
		0, 0, 1, 0x06, 0x05, 0x01, // SEI: starts the next AU
		0, 0, 0, 1, 0x41, 0x80, 0x33, // next picture
	}
	path := filepath.Join(t.TempDir(), "two.h264")
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	aus := testutil.LoadAnnexB(t, path)
	if len(aus) != 2 {
		t.Fatalf("got %d AUs, want 2", len(aus))
	}
	if len(aus[0].NALUs) != 2 || len(aus[1].NALUs) != 2 {
		t.Fatalf("NALU counts %d,%d, want 2,2", len(aus[0].NALUs), len(aus[1].NALUs))
	}
	if aus[0].IDR || aus[1].IDR {
		t.Fatal("no AU should be IDR")
	}
}
