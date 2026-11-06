package zrec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func validHeaderBytes(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := NewWriter(&out, testParams(t, 65536)); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestParseHeaderIgnoresTrailingBody(t *testing.T) {
	b := validHeaderBytes(t)
	h, err := ParseHeader(append(bytes.Clone(b), 1, 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if int(h.HeaderLen) != len(b) {
		t.Fatalf("HeaderLen = %d, want %d", h.HeaderLen, len(b))
	}
}

func TestParseHeaderShortInputIsTruncated(t *testing.T) {
	b := validHeaderBytes(t)
	for _, n := range []int{0, 3, 36, 37, len(b) - 1} {
		if _, err := ParseHeader(b[:n]); !errors.Is(err, ErrTruncated) {
			t.Fatalf("ParseHeader(%d bytes) = %v, want ErrTruncated", n, err)
		}
	}
}

func TestParseHeaderFormatErrors(t *testing.T) {
	n := len("front-door")
	cases := map[string]func(b []byte){
		"magic":            func(b []byte) { b[0] = 'X' },
		"format_version":   func(b []byte) { b[4] = 2 },
		"suite":            func(b []byte) { b[5] = 0 },
		"header_len":       func(b []byte) { binary.BigEndian.PutUint16(b[6:8], uint16(106+n)) },
		"chunk_size small": func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 65535) },
		"chunk_size pow2":  func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 100000) },
		"chunk_size large": func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 1<<25) },
		"ulid timestamp":   func(b []byte) { b[17]++ },
		"start_unix_ms":    func(b []byte) { b[35]++ },
		"camera_id_len 0":  func(b []byte) { b[36] = 0 },
		"camera_id char":   func(b []byte) { b[37] = 'F' },
		"camera_id dash":   func(b []byte) { b[37] = '-' },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := validHeaderBytes(t)
			mutate(b)
			if _, err := ParseHeader(b); !errors.Is(err, ErrFormat) {
				t.Fatalf("ParseHeader = %v, want ErrFormat", err)
			}
		})
	}
}

func TestAuthErrorIsErrAuth(t *testing.T) {
	var err error = &AuthError{Chunk: 3}
	if !errors.Is(err, ErrAuth) {
		t.Fatal("AuthError is not ErrAuth")
	}
	if errors.Is(err, ErrFormat) {
		t.Fatal("AuthError matched ErrFormat")
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Chunk != 3 {
		t.Fatalf("errors.As = %v", ae)
	}
	if err.Error() == "" {
		t.Fatal("empty message")
	}
	sentinels := []error{ErrFormat, ErrUnknownKey, ErrAuthHeader, ErrTruncated, ErrTrailingData, ErrEmpty, ErrAuth}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if (i == j) != errors.Is(a, b) {
				t.Fatalf("errors.Is(%v, %v) = %v", a, b, errors.Is(a, b))
			}
		}
	}
}
