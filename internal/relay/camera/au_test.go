package camera_test

import (
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

func TestCodecEqual(t *testing.T) {
	t.Parallel()
	base := camera.Codec{Kind: "h265", VPS: []byte{1}, SPS: []byte{2, 3}, PPS: []byte{4}, Width: 640, Height: 360}
	same := camera.Codec{Kind: "h265", VPS: []byte{1}, SPS: []byte{2, 3}, PPS: []byte{4}, Width: 640, Height: 360}
	if !base.Equal(same) {
		t.Fatal("identical codecs not Equal")
	}
	for name, mod := range map[string]func(*camera.Codec){
		"kind":   func(c *camera.Codec) { c.Kind = "h264" },
		"vps":    func(c *camera.Codec) { c.VPS = []byte{9} },
		"sps":    func(c *camera.Codec) { c.SPS = []byte{2, 4} },
		"pps":    func(c *camera.Codec) { c.PPS = nil },
		"width":  func(c *camera.Codec) { c.Width = 1280 },
		"height": func(c *camera.Codec) { c.Height = 720 },
	} {
		c := same
		mod(&c)
		if base.Equal(c) {
			t.Errorf("codecs differing in %s are Equal", name)
		}
	}
}
