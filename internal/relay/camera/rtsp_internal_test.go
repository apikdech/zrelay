package camera

import (
	"errors"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

func g711Media(mulaw bool, rate int) *description.Media {
	pt := uint8(8)
	if mulaw {
		pt = 0
	}
	return &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{
		&format.G711{PayloadTyp: pt, MULaw: mulaw, SampleRate: rate, ChannelCount: 1},
	}}
}

func videoMedia(f format.Format) *description.Media {
	return &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{f}}
}

func TestSelectVideo(t *testing.T) {
	t.Parallel()
	h264f := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	h265f := &format.H265{PayloadTyp: 97}

	_, f, err := selectVideo(&description.Session{Medias: []*description.Media{videoMedia(h265f), videoMedia(h264f)}})
	if err != nil || f != h264f {
		t.Fatalf("H.265 + H.264 selected %v, %v; want the H.264 track", f, err)
	}
	_, f, err = selectVideo(&description.Session{Medias: []*description.Media{videoMedia(h265f), g711Media(false, 8000)}})
	if err != nil || f != h265f {
		t.Fatalf("H.265 only selected %v, %v", f, err)
	}
	_, _, err = selectVideo(&description.Session{Medias: []*description.Media{
		g711Media(true, 8000),
		videoMedia(&format.MJPEG{}),
	}})
	if !errors.Is(err, errNoVideo) {
		t.Fatalf("no H.264/H.265: err %v, want errNoVideo", err)
	}
}

func TestSelectAudio(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		media *description.Media
		want  string
	}{
		{"PCMA", g711Media(false, 8000), "PCMA"},
		{"PCMU", g711Media(true, 8000), "PCMU"},
		{"16 kHz", g711Media(false, 16000), ""},
	} {
		desc := &description.Session{Medias: []*description.Media{videoMedia(&format.H264{PayloadTyp: 96}), tc.media}}
		medi, _, codec := selectAudio(desc)
		if codec != tc.want || (medi != nil) != (tc.want != "") {
			t.Errorf("%s: codec %q (media %v), want %q", tc.name, codec, medi != nil, tc.want)
		}
	}
	if medi, _, codec := selectAudio(&description.Session{}); medi != nil || codec != "" {
		t.Errorf("no audio: codec %q", codec)
	}
}
