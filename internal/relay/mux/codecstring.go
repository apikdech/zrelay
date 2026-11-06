package mux

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

// CodecString returns the RFC 6381 codec string recorded in the sidecar so
// viewers can call MediaSource.isTypeSupported (Contract §5). H.264 gives
// avc1.PPCCLL from SPS bytes 1 to 3 (profile_idc, constraint flags,
// level_idc); H.265 gives hvc1.<profile>.<compat>.<tier><level>.<constraints>
// from the SPS profile_tier_level (ISO/IEC 14496-15 Annex E).
func CodecString(c camera.Codec) (string, error) {
	switch c.Kind {
	case "h264":
		if len(c.SPS) < 4 {
			return "", errors.New("mux: h264 SPS too short for a codec string")
		}
		return fmt.Sprintf("avc1.%02x%02x%02x", c.SPS[1], c.SPS[2], c.SPS[3]), nil
	case "h265":
		return hevcCodecString(c.SPS)
	default:
		return "", fmt.Errorf("mux: unsupported codec kind %q", c.Kind)
	}
}

// hevcCodecString parses general_profile_space .. general_level_idc, which
// start 3 bytes into the SPS NAL unit (2-byte header, then vps_id,
// max_sub_layers and nesting flag in one byte).
func hevcCodecString(sps []byte) (string, error) {
	raw := h264.EmulationPreventionRemove(sps)
	// header(2) + 1 + profile(1) + compat(4) + constraints(6) + level(1)
	const need = 2 + 1 + 1 + 4 + 6 + 1
	if len(raw) < need {
		return "", errors.New("mux: h265 SPS too short for a codec string")
	}
	ptl := raw[3:]
	space := ptl[0] >> 6
	tier := "L"
	if ptl[0]&0x20 != 0 {
		tier = "H"
	}
	profile := ptl[0] & 0x1f
	compat := bits.Reverse32(uint32(ptl[1])<<24 | uint32(ptl[2])<<16 | uint32(ptl[3])<<8 | uint32(ptl[4]))
	constraints := ptl[5:11]
	level := ptl[11]

	var sb strings.Builder
	sb.WriteString("hvc1.")
	if space > 0 {
		sb.WriteByte('A' + space - 1)
	}
	fmt.Fprintf(&sb, "%d.%x.%s%d", profile, compat, tier, level)
	// Trailing zero constraint bytes are omitted.
	n := len(constraints)
	for n > 0 && constraints[n-1] == 0 {
		n--
	}
	for _, b := range constraints[:n] {
		fmt.Fprintf(&sb, ".%X", b)
	}
	return sb.String(), nil
}
