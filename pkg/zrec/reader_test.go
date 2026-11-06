package zrec

import (
	"bytes"
	"errors"
	"testing"
)

const readerCS = 1 << 16

func testRing() Keyring {
	k := Derive(fixedMaster())
	return Keyring{k.ID: k}
}

// clip3 is a 2.5-chunk clip: chunks 0 and 1 full, chunk 2 half full.
func clip3(t *testing.T) (plain, file []byte, headerLen int) {
	t.Helper()
	plain = pattern(readerCS * 5 / 2)
	w, file := writeClip(t, testParams(t, readerCS), plain, 4093)
	return plain, file, w.HeaderLen()
}

func openBytes(t *testing.T, file []byte, opt ReaderOptions) (*Reader, error) {
	t.Helper()
	return OpenReader(bytes.NewReader(file), int64(len(file)), testRing(), opt)
}

func mustOpen(t *testing.T, file []byte, opt ReaderOptions) *Reader {
	t.Helper()
	r, err := openBytes(t, file, opt)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	return r
}

func TestReaderReadsWholeClip(t *testing.T) {
	plain, file, hl := clip3(t)
	r := mustOpen(t, file, ReaderOptions{})
	if got := r.NumChunks(); got != 3 {
		t.Fatalf("NumChunks = %d, want 3", got)
	}
	h := r.Header()
	if h.CameraID != "front-door" || h.ChunkSize != readerCS || int(h.HeaderLen) != hl || h.StartUnixMs != testStartMs {
		t.Fatalf("Header = %+v", h)
	}
	var out bytes.Buffer
	n, err := r.WriteTo(&out)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != int64(len(plain)) || !bytes.Equal(out.Bytes(), plain) {
		t.Fatalf("WriteTo: n=%d, plaintext mismatch", n)
	}
}

func TestReaderHeaderValidationErrors(t *testing.T) {
	_, file, hl := clip3(t)
	flip := func(off int, mask byte) []byte {
		b := bytes.Clone(file)
		b[off] ^= mask
		return b
	}
	tests := []struct {
		name string
		file []byte
		want error
	}{
		{"bad magic", flip(0, 0x01), ErrFormat},
		{"bad version", flip(4, 0x02), ErrFormat},
		{"bad suite", flip(5, 0x02), ErrFormat},
		{"header_len mismatch", flip(7, 0x01), ErrFormat},
		{"invalid camera_id", flip(37, 0x20), ErrFormat}, // 'f' -> 'F'
		{"clip_id timestamp", flip(13, 0x01), ErrFormat},
		{"start_unix_ms", flip(35, 0x01), ErrFormat},
		{"clip_id entropy", flip(20, 0x01), ErrAuthHeader},
		{"key_id", flip(hl-68, 0x01), ErrUnknownKey},
		{"wrap_nonce", flip(hl-60, 0x01), ErrAuthHeader},
		{"wrapped_dek", flip(hl-1, 0x01), ErrAuthHeader},
		{"short header", file[:hl-1], ErrTruncated},
		{"camera_id_len 100", oversizedCameraIDLen(t), ErrFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := openBytes(t, tc.file, ReaderOptions{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("OpenReader error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReaderOrderUnknownKeyBeforeUnwrap(t *testing.T) {
	_, file, hl := clip3(t)
	b := bytes.Clone(file)
	b[hl-68] ^= 0x01 // key_id
	b[hl-1] ^= 0x01  // wrapped_dek tag
	_, err := openBytes(t, b, ReaderOptions{})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("error = %v, want ErrUnknownKey", err)
	}
	if errors.Is(err, ErrAuthHeader) {
		t.Fatalf("error = %v also matches ErrAuthHeader", err)
	}

	// An empty keyring reports ErrUnknownKey even for an intact clip.
	if _, err := OpenReader(bytes.NewReader(file), int64(len(file)), nil, ReaderOptions{}); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("nil keyring: error = %v, want ErrUnknownKey", err)
	}

	// Format errors come before the keyring lookup.
	b[0] ^= 0x01
	if _, err := openBytes(t, b, ReaderOptions{}); !errors.Is(err, ErrFormat) {
		t.Fatalf("bad magic + bad key_id: error = %v, want ErrFormat", err)
	}
}

func TestAllowIncompleteLastChunk(t *testing.T) {
	_, file, _ := clip3(t)
	cut := file[:len(file)-1000] // mid-final-chunk; still >= 17 bytes left in it

	r := mustOpen(t, cut, ReaderOptions{})
	n := r.NumChunks()
	if n != 3 {
		t.Fatalf("NumChunks = %d, want 3", n)
	}
	plain, _, err := r.ReadChunk(n-1, nil)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Chunk != n-1 {
		t.Fatalf("default ReadChunk(%d) error = %v, want ErrAuth(%d)", n-1, err, n-1)
	}
	if plain != nil {
		t.Fatalf("plaintext released for a chunk that failed authentication")
	}
	if _, err := r.WriteTo(&bytes.Buffer{}); !errors.As(err, &ae) || ae.Chunk != n-1 {
		t.Fatalf("default WriteTo error = %v, want ErrAuth(%d)", err, n-1)
	}

	r = mustOpen(t, cut, ReaderOptions{AllowIncomplete: true})
	if _, _, err := r.ReadChunk(n-1, nil); !errors.Is(err, ErrTruncated) || errors.Is(err, ErrAuth) {
		t.Fatalf("AllowIncomplete ReadChunk error = %v, want ErrTruncated", err)
	}
	var out bytes.Buffer
	if _, err := r.WriteTo(&out); !errors.Is(err, ErrTruncated) {
		t.Fatalf("AllowIncomplete WriteTo error = %v, want ErrTruncated", err)
	}
	if out.Len() != 2*readerCS {
		t.Fatalf("AllowIncomplete WriteTo released %d bytes, want the %d verified ones", out.Len(), 2*readerCS)
	}

	// AllowIncomplete only relaxes the last chunk.
	b := bytes.Clone(cut)
	b[len(b)-readerCS-500] ^= 0x01 // inside chunk 1
	r = mustOpen(t, b, ReaderOptions{AllowIncomplete: true})
	if _, _, err := r.ReadChunk(1, nil); !errors.As(err, &ae) || ae.Chunk != 1 {
		t.Fatalf("AllowIncomplete ReadChunk(1) error = %v, want ErrAuth(1)", err)
	}
}

func TestRandomAccess(t *testing.T) {
	plain, file, _ := clip3(t)
	r := mustOpen(t, file, ReaderOptions{})
	want := func(i int) []byte {
		return plain[i*readerCS : min((i+1)*readerCS, len(plain))]
	}
	for _, i := range []int{2, 0, 1, 2} {
		got, final, err := r.ReadChunk(i, nil)
		if err != nil {
			t.Fatalf("ReadChunk(%d): %v", i, err)
		}
		if final != (i == 2) {
			t.Fatalf("ReadChunk(%d) final = %v", i, final)
		}
		if !bytes.Equal(got, want(i)) {
			t.Fatalf("ReadChunk(%d) plaintext mismatch", i)
		}
	}

	// dst is reused when it is large enough.
	dst := make([]byte, 0, readerCS+16)
	got, _, err := r.ReadChunk(1, dst)
	if err != nil || !bytes.Equal(got, want(1)) {
		t.Fatalf("ReadChunk with dst: err=%v", err)
	}
	if &got[0] != &dst[:1][0] {
		t.Fatalf("ReadChunk did not decrypt into dst")
	}

	for _, i := range []int{-1, 3} {
		if _, _, err := r.ReadChunk(i, nil); err == nil {
			t.Fatalf("ReadChunk(%d) succeeded out of range", i)
		}
	}
}

func TestChunkClassification(t *testing.T) {
	plain, file, hl := clip3(t)
	const s = readerCS + 16
	chunk := func(i int) []byte { return file[hl+i*s : min(hl+(i+1)*s, len(file))] }

	twoFull := func() []byte { // a 2-chunk clip, both chunks full-size
		_, f := writeClip(t, testParams(t, readerCS), plain[:2*readerCS], 1<<20)
		return f
	}

	tests := []struct {
		name string
		file []byte
		want error
		at   int // chunk index for ErrAuth
	}{
		{"header only", file[:hl], ErrTruncated, 0},
		{"last chunk under 17 bytes", file[:hl+2*s+16], ErrTruncated, 0},
		{"dropped final chunk", file[:hl+2*s], ErrTruncated, 0},
		{"extra whole chunk", append(twoFull(), chunk(0)...), ErrTrailingData, 0},
		{"100 trailing bytes", append(bytes.Clone(file), make([]byte, 100)...), ErrAuth, 2},
		{"swapped chunks", append(append(append(bytes.Clone(file[:hl]), chunk(1)...), chunk(0)...), chunk(2)...), ErrAuth, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := mustOpen(t, tc.file, ReaderOptions{})
			var out bytes.Buffer
			_, err := r.WriteTo(&out)
			if !errors.Is(err, tc.want) {
				t.Fatalf("WriteTo error = %v, want %v", err, tc.want)
			}
			if tc.want == ErrAuth {
				var ae *AuthError
				if !errors.As(err, &ae) || ae.Chunk != tc.at {
					t.Fatalf("WriteTo error = %v, want ErrAuth(%d)", err, tc.at)
				}
			}
		})
	}
}

func TestOpenReaderSizeBeyondData(t *testing.T) {
	_, file, _ := clip3(t)
	r, err := OpenReader(bytes.NewReader(file), int64(len(file))+(readerCS+16), testRing(), ReaderOptions{})
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if _, _, err := r.ReadChunk(3, nil); err == nil {
		t.Fatal("ReadChunk past the real data succeeded")
	}
	if _, err := OpenReader(bytes.NewReader(file), -1, testRing(), ReaderOptions{}); err == nil {
		t.Fatal("negative size accepted")
	}
}
