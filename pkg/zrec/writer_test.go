package zrec

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// Test hooks: reach into the writer without adding test-only code to the
// production files.
func (w *Writer) dekForTest() [32]byte { return w.dek }

func (w *Writer) bufferCapForTest() int { return cap(w.pt) + cap(w.out) }

const testStartMs = int64(1789570740000) // 2026-09-16T14:59:00Z

// countingReader is a deterministic Rand that yields an incrementing byte
// pattern, so tests can predict the DEK and wrap nonce.
type countingReader struct{ n byte }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

func testClipID(t *testing.T, ms int64) ulid.ULID {
	t.Helper()
	var id ulid.ULID
	if err := id.SetTime(uint64(ms)); err != nil {
		t.Fatal(err)
	}
	for i := 6; i < 16; i++ {
		id[i] = byte(0xA0 + i)
	}
	return id
}

func testParams(t *testing.T, cs int) WriterParams {
	t.Helper()
	return WriterParams{
		Keys:        Derive(fixedMaster()),
		ClipID:      testClipID(t, testStartMs),
		StartUnixMs: testStartMs,
		CameraID:    "front-door",
		ChunkSize:   cs,
		Rand:        &countingReader{},
	}
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

// writeClip writes plaintext in pieces of the given size and closes.
func writeClip(t *testing.T, p WriterParams, plaintext []byte, piece int) (*Writer, []byte) {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, p)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for off := 0; off < len(plaintext); off += piece {
		end := min(off+piece, len(plaintext))
		n, err := w.Write(plaintext[off:end])
		if err != nil || n != end-off {
			t.Fatalf("Write: n=%d err=%v", n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return w, out.Bytes()
}

func TestWriterRejectsClipIDTimestampMismatch(t *testing.T) {
	p := testParams(t, 65536)
	p.ClipID = testClipID(t, testStartMs+1)
	var out bytes.Buffer
	if _, err := NewWriter(&out, p); err == nil {
		t.Fatal("NewWriter accepted a clip_id whose timestamp differs from start_unix_ms")
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %d bytes on rejected params", out.Len())
	}
}

func TestWriterRejectsBadChunkSize(t *testing.T) {
	for _, cs := range []int{65535, 1<<24 + 1, 100000, 1 << 15, 1 << 25, -65536} {
		t.Run(fmt.Sprint(cs), func(t *testing.T) {
			var out bytes.Buffer
			if _, err := NewWriter(&out, testParams(t, cs)); err == nil {
				t.Fatalf("NewWriter accepted chunk_size %d", cs)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote %d bytes on rejected params", out.Len())
			}
		})
	}
	for _, cs := range []int{1 << 16, 1 << 20, 1 << 24} {
		var out bytes.Buffer
		if _, err := NewWriter(&out, testParams(t, cs)); err != nil {
			t.Fatalf("NewWriter rejected valid chunk_size %d: %v", cs, err)
		}
	}
}

func TestWriterDefaultChunkSize(t *testing.T) {
	var out bytes.Buffer
	if _, err := NewWriter(&out, testParams(t, 0)); err != nil {
		t.Fatal(err)
	}
	h, err := ParseHeader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if h.ChunkSize != 1048576 {
		t.Fatalf("default chunk_size = %d, want 1048576", h.ChunkSize)
	}
}

func TestWriterRejectsBadCameraID(t *testing.T) {
	for _, id := range []string{"", "Front", "-front", "front_door", "front door", "fröнт", strings.Repeat("a", 65)} {
		t.Run(id, func(t *testing.T) {
			p := testParams(t, 65536)
			p.CameraID = id
			var out bytes.Buffer
			if _, err := NewWriter(&out, p); err == nil {
				t.Fatalf("NewWriter accepted camera_id %q", id)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote %d bytes on rejected params", out.Len())
			}
		})
	}
	for _, id := range []string{"a", "0", "front-door", "cam-1-", strings.Repeat("a", 64)} {
		p := testParams(t, 65536)
		p.CameraID = id
		var out bytes.Buffer
		if _, err := NewWriter(&out, p); err != nil {
			t.Fatalf("NewWriter rejected valid camera_id %q: %v", id, err)
		}
	}
}

func TestHeaderLayout(t *testing.T) {
	p := testParams(t, 65536)
	var out bytes.Buffer
	w, err := NewWriter(&out, p)
	if err != nil {
		t.Fatal(err)
	}
	n := len(p.CameraID)
	b := out.Bytes()
	if len(b) != 105+n || w.HeaderLen() != 105+n {
		t.Fatalf("header written = %d bytes, HeaderLen() = %d, want %d", len(b), w.HeaderLen(), 105+n)
	}
	if string(b[0:4]) != "ZREC" {
		t.Fatalf("magic = %q", b[0:4])
	}
	if b[4] != 1 || b[5] != 1 {
		t.Fatalf("format_version, suite = %d, %d", b[4], b[5])
	}
	if got := binary.BigEndian.Uint16(b[6:8]); int(got) != 105+n {
		t.Fatalf("header_len = %d, want %d", got, 105+n)
	}
	if got := binary.BigEndian.Uint32(b[8:12]); got != 65536 {
		t.Fatalf("chunk_size = %d", got)
	}
	if !bytes.Equal(b[12:28], p.ClipID[:]) {
		t.Fatalf("clip_id = %x", b[12:28])
	}
	if got := int64(binary.BigEndian.Uint64(b[28:36])); got != testStartMs {
		t.Fatalf("start_unix_ms = %d", got)
	}
	if int(b[36]) != n || string(b[37:37+n]) != p.CameraID {
		t.Fatalf("camera_id = %d %q", b[36], b[37:37+n])
	}
	k := b[37+n:]
	if !bytes.Equal(k[0:8], p.Keys.ID[:]) {
		t.Fatalf("key_id = %x, want %x", k[0:8], p.Keys.ID)
	}

	// The counting Rand yields the DEK first (0x00..0x1f), then the wrap
	// nonce (0x20..0x2b).
	var wantNonce [12]byte
	for i := range wantNonce {
		wantNonce[i] = byte(0x20 + i)
	}
	if !bytes.Equal(k[8:20], wantNonce[:]) {
		t.Fatalf("wrap_nonce = %x", k[8:20])
	}

	// wrapped_dek = Seal(K_wrap, wrap_nonce, DEK, AAD = H_D || key_id).
	hd := sha256.Sum256(b[:37+n])
	aad := append(hd[:], p.Keys.ID[:]...)
	dek, err := gcmFor(t, p.Keys.Wrap[:]).Open(nil, k[8:20], k[20:68], aad)
	if err != nil {
		t.Fatalf("unwrap DEK: %v", err)
	}
	for i, c := range dek {
		if c != byte(i) {
			t.Fatalf("DEK = %x, want 00..1f", dek)
		}
	}

	h, err := ParseHeader(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if !bytes.Equal(h.Descriptor(), b[:37+n]) {
		t.Fatal("Descriptor() differs from the written descriptor")
	}
	want := Header{
		FormatVersion: 1, Suite: 1, HeaderLen: uint16(105 + n), ChunkSize: 65536,
		ClipID: p.ClipID, StartUnixMs: testStartMs, CameraID: p.CameraID,
		KeyID: p.Keys.ID, WrapNonce: wantNonce,
	}
	copy(want.WrappedDEK[:], k[20:68])
	if h != want {
		t.Fatalf("ParseHeader = %+v\nwant %+v", h, want)
	}
	if MaxHeaderLen != 169 {
		t.Fatalf("MaxHeaderLen = %d", MaxHeaderLen)
	}
}

func TestWriterUsesSuppliedDEK(t *testing.T) {
	p := testParams(t, 65536)
	var dek [32]byte
	for i := range dek {
		dek[i] = 0xD0 ^ byte(i)
	}
	p.DEK = &dek
	var out bytes.Buffer
	if _, err := NewWriter(&out, p); err != nil {
		t.Fatal(err)
	}
	h, err := ParseHeader(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// With a supplied DEK, the first Rand bytes become the wrap nonce.
	for i, c := range h.WrapNonce {
		if c != byte(i) {
			t.Fatalf("wrap_nonce = %x, want 00..0b", h.WrapNonce)
		}
	}
	got := unwrapForTest(t, h, p.Keys)
	if !bytes.Equal(got, dek[:]) {
		t.Fatalf("wrapped DEK = %x, want %x", got, dek)
	}
}

func unwrapForTest(t *testing.T, h Header, k Keys) []byte {
	t.Helper()
	hd := sha256.Sum256(h.Descriptor())
	dek, err := gcmFor(t, k.Wrap[:]).Open(nil, h.WrapNonce[:], h.WrappedDEK[:], append(hd[:], h.KeyID[:]...))
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	return dek
}

func gcmFor(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	blk, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// openChunks decrypts the body per Contract §4.2 and returns the chunk
// plaintexts, failing on any deviation from the nonce/AAD construction.
func openChunks(t *testing.T, file []byte, k Keys) [][]byte {
	t.Helper()
	h, err := ParseHeader(file)
	if err != nil {
		t.Fatal(err)
	}
	g := gcmFor(t, unwrapForTest(t, h, k))
	hd := sha256.Sum256(h.Descriptor())
	body := file[h.HeaderLen:]
	s := int(h.ChunkSize) + 16
	var out [][]byte
	for i := 0; len(body) > 0; i++ {
		end := min(s, len(body))
		var nonce [12]byte
		binary.BigEndian.PutUint64(nonce[3:11], uint64(i))
		if end == len(body) {
			nonce[11] = 1
		}
		pt, err := g.Open(nil, nonce[:], body[:end], hd[:])
		if err != nil {
			t.Fatalf("chunk %d (final=%d): %v", i, nonce[11], err)
		}
		if nonce[11] == 0 && len(pt) != int(h.ChunkSize) {
			t.Fatalf("non-final chunk %d holds %d bytes", i, len(pt))
		}
		out = append(out, pt)
		body = body[end:]
	}
	return out
}

func TestHoldBackExactChunkIsSingleFinalChunk(t *testing.T) {
	const cs = 65536
	p := testParams(t, cs)
	plain := pattern(cs)
	w, file := writeClip(t, p, plain, 1000)
	body := file[w.HeaderLen():]
	if len(body) != cs+16 {
		t.Fatalf("body = %d bytes, want %d", len(body), cs+16)
	}
	chunks := openChunks(t, file, p.Keys)
	if len(chunks) != 1 || !bytes.Equal(chunks[0], plain) {
		t.Fatalf("got %d chunks", len(chunks))
	}
}

func TestTwoAndHalfChunks(t *testing.T) {
	const cs = 65536
	for _, piece := range []int{1, 777, cs, cs + 1, 3 * cs} {
		t.Run(fmt.Sprint(piece), func(t *testing.T) {
			p := testParams(t, cs)
			plain := pattern(2*cs + cs/2)
			w, file := writeClip(t, p, plain, piece)
			body := file[w.HeaderLen():]
			if want := 2*(cs+16) + (cs/2 + 16); len(body) != want {
				t.Fatalf("body = %d bytes, want %d", len(body), want)
			}
			chunks := openChunks(t, file, p.Keys)
			if len(chunks) != 3 || !bytes.Equal(bytes.Join(chunks, nil), plain) {
				t.Fatalf("got %d chunks, plaintext mismatch", len(chunks))
			}
		})
	}
}

func TestEmptyCloseFails(t *testing.T) {
	var out bytes.Buffer
	w, err := NewWriter(&out, testParams(t, 65536))
	if err != nil {
		t.Fatal(err)
	}
	hl := out.Len()
	if _, err := w.Write(nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Close on empty plaintext = %v, want ErrEmpty", err)
	}
	if out.Len() != hl {
		t.Fatalf("Close wrote %d bytes after ErrEmpty", out.Len()-hl)
	}
	if w.dekForTest() != ([32]byte{}) {
		t.Fatal("DEK not zeroed after failed Close")
	}
}

func TestPeakBufferBound(t *testing.T) {
	const cs = 65536
	bound := 2*cs + 17
	p := testParams(t, cs)
	var out bytes.Buffer
	w, err := NewWriter(&out, p)
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		if c := w.bufferCapForTest(); c > bound {
			t.Fatalf("%s: buffer cap %d > %d", when, c, bound)
		}
	}
	check("after NewWriter")
	plain := pattern(5*cs + 3)
	for off, piece := 0, 1; off < len(plain); piece = piece*3 + 1 {
		end := min(off+piece, len(plain))
		if _, err := w.Write(plain[off:end]); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprintf("after write to %d", end))
		off = end
	}
	if _, err := w.Write(pattern(4 * cs)); err != nil {
		t.Fatal(err)
	}
	check("after large write")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	check("after Close")
}

func TestAbortZeroesDEK(t *testing.T) {
	var out bytes.Buffer
	w, err := NewWriter(&out, testParams(t, 65536))
	if err != nil {
		t.Fatal(err)
	}
	if w.dekForTest() == ([32]byte{}) {
		t.Fatal("DEK unexpectedly zero before Abort")
	}
	if _, err := w.Write(pattern(1000)); err != nil {
		t.Fatal(err)
	}
	n := out.Len()
	w.Abort()
	if w.dekForTest() != ([32]byte{}) {
		t.Fatal("DEK not zeroed by Abort")
	}
	if w.bufferCapForTest() != 0 {
		t.Fatal("buffers not dropped by Abort")
	}
	if _, err := w.Write([]byte{1}); err == nil {
		t.Fatal("Write after Abort succeeded")
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close after Abort succeeded")
	}
	if out.Len() != n {
		t.Fatal("bytes written after Abort")
	}
}

func TestCloseZeroesDEK(t *testing.T) {
	w, _ := writeClip(t, testParams(t, 65536), pattern(10), 10)
	if w.dekForTest() != ([32]byte{}) {
		t.Fatal("DEK not zeroed after Close")
	}
	if err := w.Close(); err == nil {
		t.Fatal("second Close succeeded")
	}
}

type failWriter struct{ after int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, io.ErrClosedPipe
	}
	f.after--
	return len(p), nil
}

func TestWriterSinkErrorIsSticky(t *testing.T) {
	w, err := NewWriter(&failWriter{after: 1}, testParams(t, 65536))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(pattern(65537)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write = %v, want sink error", err)
	}
	if _, err := w.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("second Write = %v, want sticky sink error", err)
	}
	if err := w.Close(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Close = %v, want sticky sink error", err)
	}
}

func TestNewWriterSinkErrorOnHeader(t *testing.T) {
	if _, err := NewWriter(&failWriter{}, testParams(t, 65536)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("NewWriter = %v, want sink error", err)
	}
}
