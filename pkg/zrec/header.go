package zrec

import (
	"encoding/binary"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// Format constants (Contract §4.1).
const (
	magic         = "ZREC"
	formatVersion = 1
	suiteV1       = 1

	descriptorFixedLen = 37
	keyBlockLen        = 68
	maxCameraIDLen     = 64

	minChunkSize     = 1 << 16
	maxChunkSize     = 1 << 24
	defaultChunkSize = 1 << 20

	// MaxHeaderLen is the largest possible header_len (105 + 64).
	MaxHeaderLen = descriptorFixedLen + maxCameraIDLen + keyBlockLen
)

// Header is a parsed zrec header: descriptor D followed by key block K.
type Header struct {
	FormatVersion, Suite uint8
	HeaderLen            uint16
	ChunkSize            uint32
	ClipID               ulid.ULID
	StartUnixMs          int64
	CameraID             string
	KeyID                [8]byte
	WrapNonce            [12]byte
	WrappedDEK           [48]byte
}

// Descriptor returns the serialized descriptor D, whose SHA-256 is the AAD
// for every chunk and part of the wrap AAD (Contract §4.2).
func (h Header) Descriptor() []byte {
	b := make([]byte, 0, descriptorFixedLen+len(h.CameraID))
	b = append(b, magic...)
	b = append(b, h.FormatVersion, h.Suite)
	b = binary.BigEndian.AppendUint16(b, h.HeaderLen)
	b = binary.BigEndian.AppendUint32(b, h.ChunkSize)
	b = append(b, h.ClipID[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(h.StartUnixMs))
	b = append(b, byte(len(h.CameraID)))
	b = append(b, h.CameraID...)
	return b
}

// marshal returns the full header, D || K.
func (h Header) marshal() []byte {
	b := h.Descriptor()
	b = append(b, h.KeyID[:]...)
	b = append(b, h.WrapNonce[:]...)
	b = append(b, h.WrappedDEK[:]...)
	return b
}

// ParseHeader parses and format-checks the header at the start of b; bytes
// after header_len are ignored. It does not consult a keyring or unwrap the
// DEK. Input shorter than the header yields ErrTruncated; anything malformed
// yields ErrFormat.
func ParseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < descriptorFixedLen {
		return h, fmt.Errorf("%w: header needs at least %d bytes, got %d", ErrTruncated, descriptorFixedLen, len(b))
	}
	if string(b[0:4]) != magic {
		return h, fmt.Errorf("%w: bad magic", ErrFormat)
	}
	h.FormatVersion, h.Suite = b[4], b[5]
	if h.FormatVersion != formatVersion || h.Suite != suiteV1 {
		return h, fmt.Errorf("%w: unsupported format_version %d / suite %d", ErrFormat, h.FormatVersion, h.Suite)
	}
	h.HeaderLen = binary.BigEndian.Uint16(b[6:8])
	n := int(b[36])
	if int(h.HeaderLen) != descriptorFixedLen+n+keyBlockLen {
		return h, fmt.Errorf("%w: header_len %d inconsistent with camera_id_len %d", ErrFormat, h.HeaderLen, n)
	}
	if n < 1 || n > maxCameraIDLen {
		return h, fmt.Errorf("%w: camera_id_len %d out of range 1..%d", ErrFormat, n, maxCameraIDLen)
	}
	if len(b) < int(h.HeaderLen) {
		return h, fmt.Errorf("%w: header needs %d bytes, got %d", ErrTruncated, h.HeaderLen, len(b))
	}
	h.ChunkSize = binary.BigEndian.Uint32(b[8:12])
	if !validChunkSize(int64(h.ChunkSize)) {
		return h, fmt.Errorf("%w: chunk_size %d", ErrFormat, h.ChunkSize)
	}
	copy(h.ClipID[:], b[12:28])
	h.StartUnixMs = int64(binary.BigEndian.Uint64(b[28:36]))
	h.CameraID = string(b[37 : 37+n])
	if !validCameraID(h.CameraID) {
		return h, fmt.Errorf("%w: invalid camera_id", ErrFormat)
	}
	if !clipIDMatches(h.ClipID, h.StartUnixMs) {
		return h, fmt.Errorf("%w: clip_id timestamp does not equal start_unix_ms", ErrFormat)
	}
	k := b[37+n:]
	copy(h.KeyID[:], k[0:8])
	copy(h.WrapNonce[:], k[8:20])
	copy(h.WrappedDEK[:], k[20:68])
	return h, nil
}

// validChunkSize reports whether cs is a power of two in [2^16, 2^24].
func validChunkSize(cs int64) bool {
	return cs >= minChunkSize && cs <= maxChunkSize && cs&(cs-1) == 0
}

// validCameraID matches ^[a-z0-9][a-z0-9-]{0,63}$.
func validCameraID(s string) bool {
	if len(s) < 1 || len(s) > maxCameraIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// clipIDMatches reports whether the 48-bit ULID timestamp equals ms.
func clipIDMatches(id ulid.ULID, ms int64) bool {
	return ms >= 0 && id.Time() == uint64(ms)
}
