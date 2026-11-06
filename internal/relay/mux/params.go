package mux

// withoutParamSets drops VPS/SPS/PPS NAL units: they are carried in avcC/hvcC
// (Contract §5), not repeated in the samples. The input is returned as is when
// it holds none.
func withoutParamSets(kind string, nalus [][]byte) [][]byte {
	var out [][]byte
	for i, n := range nalus {
		if len(n) == 0 || !isParamSet(kind, n[0]) {
			if out != nil {
				out = append(out, n)
			}
			continue
		}
		if out == nil {
			out = append(make([][]byte, 0, len(nalus)), nalus[:i]...)
		}
	}
	if out == nil {
		return nalus
	}
	return out
}

func isParamSet(kind string, header byte) bool {
	if kind == "h265" {
		t := (header >> 1) & 0x3f
		return t >= 32 && t <= 34
	}
	t := header & 0x1f
	return t == 7 || t == 8
}
