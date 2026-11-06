package zrec

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/oklog/ulid/v2"
)

// WriterParams configures a Writer (Contract §4.3).
type WriterParams struct {
	Keys        Keys
	ClipID      ulid.ULID
	StartUnixMs int64
	CameraID    string
	// ChunkSize is the plaintext chunk size; 0 selects the default 1 MiB.
	ChunkSize int
	// Rand supplies the DEK and wrap_nonce; nil means crypto/rand.
	Rand io.Reader
	// DEK fixes the data key, for test vectors only; nil draws it from Rand.
	DEK *[32]byte
}

var (
	errClosed  = errors.New("zrec: writer closed")
	errAborted = errors.New("zrec: writer aborted")
)

// Writer encrypts a clip as a zrec stream: the header first, then
// AES-256-GCM chunks of exactly chunk_size plaintext bytes, the last one
// sealed with final_flag 1. It holds at most 2·chunk_size+17 bytes of
// buffer: chunk_size+1 bytes of plaintext (one hold-back byte beyond a full
// chunk) and one sealed chunk of chunk_size+16 bytes.
type Writer struct {
	w         io.Writer
	dek       [32]byte
	aead      cipher.AEAD
	hd        [32]byte
	headerLen int
	cs        int
	pt        []byte // buffered plaintext, cap cs+1
	out       []byte // sealed chunk, cap cs+16
	chunk     uint64 // index of the next chunk to seal
	total     int64  // plaintext bytes accepted
	err       error  // sticky: sink error, errClosed or errAborted
}

// NewWriter validates p, generates the DEK (unless supplied) and wrap_nonce,
// and writes the full header to w before returning.
func NewWriter(w io.Writer, p WriterParams) (*Writer, error) {
	cs := p.ChunkSize
	if cs == 0 {
		cs = defaultChunkSize
	}
	if !validChunkSize(int64(cs)) {
		return nil, fmt.Errorf("zrec: chunk_size %d is not a power of two in [%d, %d]", cs, minChunkSize, maxChunkSize)
	}
	if !validCameraID(p.CameraID) {
		return nil, fmt.Errorf("zrec: camera_id %q does not match ^[a-z0-9][a-z0-9-]{0,63}$", p.CameraID)
	}
	if !clipIDMatches(p.ClipID, p.StartUnixMs) {
		return nil, fmt.Errorf("zrec: clip_id timestamp %d does not equal start_unix_ms %d", p.ClipID.Time(), p.StartUnixMs)
	}
	r := p.Rand
	if r == nil {
		r = rand.Reader
	}

	zw := &Writer{w: w, cs: cs}
	if p.DEK != nil {
		zw.dek = *p.DEK
	} else if _, err := io.ReadFull(r, zw.dek[:]); err != nil {
		return nil, fmt.Errorf("zrec: generate DEK: %w", err)
	}
	h := Header{
		FormatVersion: formatVersion,
		Suite:         suiteV1,
		HeaderLen:     uint16(descriptorFixedLen + len(p.CameraID) + keyBlockLen),
		ChunkSize:     uint32(cs),
		ClipID:        p.ClipID,
		StartUnixMs:   p.StartUnixMs,
		CameraID:      p.CameraID,
		KeyID:         p.Keys.ID,
	}
	if _, err := io.ReadFull(r, h.WrapNonce[:]); err != nil {
		zw.zero()
		return nil, fmt.Errorf("zrec: generate wrap_nonce: %w", err)
	}
	zw.hd = sha256.Sum256(h.Descriptor())
	zw.headerLen = int(h.HeaderLen)

	wrap := newGCM(p.Keys.Wrap[:])
	wrapAAD := append(zw.hd[:], h.KeyID[:]...)
	wrap.Seal(h.WrappedDEK[:0], h.WrapNonce[:], zw.dek[:], wrapAAD)
	zw.aead = newGCM(zw.dek[:])

	if err := writeFull(w, h.marshal()); err != nil {
		zw.zero()
		return nil, fmt.Errorf("zrec: write header: %w", err)
	}
	zw.pt = make([]byte, 0, cs+1)
	zw.out = make([]byte, 0, cs+16)
	return zw, nil
}

func newGCM(key []byte) cipher.AEAD {
	blk, err := aes.NewCipher(key)
	if err != nil {
		panic("zrec: aes: " + err.Error()) // key is always 32 bytes
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		panic("zrec: gcm: " + err.Error())
	}
	return g
}

func writeFull(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

// HeaderLen returns the length of the header written by NewWriter.
func (w *Writer) HeaderLen() int { return w.headerLen }

// Write buffers plaintext, sealing and emitting a non-final chunk each time
// more than chunk_size bytes are buffered (hold-back, Contract §4.3 rule 2).
// It returns the number of bytes of p consumed. Errors from the sink are
// sticky.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n := 0
	for len(p) > 0 {
		k := copy(w.pt[len(w.pt):cap(w.pt)], p)
		w.pt = w.pt[:len(w.pt)+k]
		p = p[k:]
		n += k
		w.total += int64(k)
		if len(w.pt) == w.cs+1 {
			if err := w.seal(w.pt[:w.cs], false); err != nil {
				return n, err
			}
			w.pt[0] = w.pt[w.cs]
			w.pt = w.pt[:1]
		}
	}
	return n, nil
}

// seal encrypts one chunk into w.out and emits it.
func (w *Writer) seal(plain []byte, final bool) error {
	var nonce [12]byte // uint88_be(i) || final_flag
	binary.BigEndian.PutUint64(nonce[3:11], w.chunk)
	if final {
		nonce[11] = 1
	}
	w.out = w.aead.Seal(w.out[:0], nonce[:], plain, w.hd[:])
	w.chunk++
	if err := writeFull(w.w, w.out); err != nil {
		w.err = fmt.Errorf("zrec: write chunk %d: %w", w.chunk-1, err)
		return w.err
	}
	return nil
}

// Close seals the buffered 1..chunk_size bytes as the final chunk and
// zeroes the DEK. If no plaintext was written it returns ErrEmpty and
// writes nothing further; the caller must not commit the object.
func (w *Writer) Close() error {
	if w.err != nil {
		err := w.err
		w.zero()
		return err
	}
	defer w.zero()
	if w.total == 0 {
		w.err = errClosed
		return ErrEmpty
	}
	if err := w.seal(w.pt, true); err != nil {
		return err
	}
	w.err = errClosed
	return nil
}

// Abort abandons the clip: it zeroes the DEK and drops the buffers. Later
// calls to Write and Close fail.
func (w *Writer) Abort() {
	if w.err == nil {
		w.err = errAborted
	}
	w.zero()
}

// zero clears key material and buffered plaintext (best effort in Go).
func (w *Writer) zero() {
	clear(w.dek[:])
	clear(w.pt[:cap(w.pt)])
	clear(w.out[:cap(w.out)])
	w.pt, w.out, w.aead = nil, nil, nil
}
