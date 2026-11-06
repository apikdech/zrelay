package zrec

import (
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// Keyring maps key_id to derived keys. Readers select the key by the
// header's key_id (Contract §3.4).
type Keyring map[[8]byte]Keys

// ReaderOptions adjusts chunk classification (Contract §4.4).
type ReaderOptions struct {
	// AllowIncomplete reports an authentication failure on the last chunk
	// as ErrTruncated rather than ErrAuth. Callers set it from
	// sidecar.status == "incomplete" or from zdecrypt --allow-incomplete.
	AllowIncomplete bool
}

// Reader decrypts a zrec clip with random access to its chunks. It is safe
// for concurrent use if the underlying io.ReaderAt is.
type Reader struct {
	r    io.ReaderAt
	size int64
	h    Header
	hd   [32]byte
	aead cipher.AEAD
	s    int64 // ciphertext chunk span, chunk_size + 16
	n    int   // number of chunks, ceil(body_len / s)
	opt  ReaderOptions
}

// OpenReader reads and validates the header of the size-byte clip in r, in
// the Contract §4.4 order: format (ErrFormat), keyring lookup
// (ErrUnknownKey), then DEK unwrap (ErrAuthHeader). size MUST be the actual
// stored length of the object, never a sidecar value. Chunk errors are
// reported by ReadChunk and WriteTo.
func OpenReader(r io.ReaderAt, size int64, ring Keyring, opt ReaderOptions) (*Reader, error) {
	if size < 0 {
		return nil, fmt.Errorf("zrec: negative size %d", size)
	}
	hb := make([]byte, min(size, MaxHeaderLen))
	if err := readFullAt(r, hb, 0); err != nil {
		return nil, fmt.Errorf("zrec: read header: %w", err)
	}
	h, err := ParseHeader(hb)
	if err != nil {
		return nil, err
	}
	zr := &Reader{r: r, size: size, h: h, s: int64(h.ChunkSize) + 16, opt: opt}
	zr.hd = sha256.Sum256(h.Descriptor())
	dek, err := unwrapDEK(h, zr.hd, ring)
	if err != nil {
		return nil, err
	}
	zr.aead = newGCM(dek[:])
	clear(dek[:])

	body := size - int64(h.HeaderLen)
	zr.n = int((body + zr.s - 1) / zr.s)
	return zr, nil
}

// readFullAt fills b from r at off; a short read is io.ErrUnexpectedEOF.
func readFullAt(r io.ReaderAt, b []byte, off int64) error {
	n, err := r.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// Header returns the validated header.
func (r *Reader) Header() Header { return r.h }

// NumChunks returns N = ceil(body_len / (chunk_size + 16)), computed from
// the object's actual size. It is 0 for a header-only object.
func (r *Reader) NumChunks() int { return r.n }

// ReadChunk authenticates and decrypts chunk i, appending its plaintext to
// dst[:0] (dst may be nil). final reports that the chunk was sealed as the
// final chunk. Plaintext is returned only after its tag verifies; on error
// plain is nil. Errors follow Contract §4.4: ErrTruncated, ErrTrailingData,
// or *AuthError (matching ErrAuth).
func (r *Reader) ReadChunk(i int, dst []byte) (plain []byte, final bool, err error) {
	return r.readChunk(i, dst, nil)
}

// readChunk is ReadChunk with a reusable ciphertext buffer.
func (r *Reader) readChunk(i int, dst, ct []byte) ([]byte, bool, error) {
	if i < 0 || i >= r.n {
		return nil, false, fmt.Errorf("zrec: chunk %d out of range [0, %d)", i, r.n)
	}
	off := int64(r.h.HeaderLen) + int64(i)*r.s
	span := min(r.s, r.size-off)
	last := i == r.n-1
	if last && span < 17 {
		return nil, false, fmt.Errorf("%w: last chunk is %d bytes", ErrTruncated, span)
	}
	if int64(cap(ct)) < span {
		ct = make([]byte, span)
	}
	ct = ct[:span]
	if err := readFullAt(r.r, ct, off); err != nil {
		return nil, false, fmt.Errorf("zrec: read chunk %d: %w", i, err)
	}

	// Try the expected flag first, then the other one to classify failure.
	if p, err := r.open(dst, ct, i, last); err == nil {
		return p, last, nil
	}
	if p, err := r.open(dst, ct, i, !last); err == nil {
		clear(p) // never release it: the flag is wrong for this position
		if last {
			// A non-final chunk at the end: the final chunk is missing.
			return nil, false, fmt.Errorf("%w: final chunk missing after chunk %d", ErrTruncated, i)
		}
		return nil, false, fmt.Errorf("%w: chunk %d is final but more data follows", ErrTrailingData, i)
	}
	if last && r.opt.AllowIncomplete {
		return nil, false, fmt.Errorf("%w: incomplete last chunk %d", ErrTruncated, i)
	}
	return nil, false, &AuthError{Chunk: i}
}

// open decrypts ct as chunk i with the given final flag. ct is left intact.
func (r *Reader) open(dst, ct []byte, i int, final bool) ([]byte, error) {
	var nonce [12]byte // uint88_be(i) || final_flag
	binary.BigEndian.PutUint64(nonce[3:11], uint64(i))
	if final {
		nonce[11] = 1
	}
	return r.aead.Open(dst[:0], nonce[:], ct, r.hd[:])
}

// WriteTo decrypts the chunks in order and writes each chunk's plaintext to
// w only after that chunk authenticates. It stops at the first error, after
// having written the plaintext of every chunk before it. A header-only
// object is ErrTruncated.
func (r *Reader) WriteTo(w io.Writer) (int64, error) {
	if r.n == 0 {
		return 0, fmt.Errorf("%w: no chunks", ErrTruncated)
	}
	ct := make([]byte, r.s)
	pt := make([]byte, 0, r.s)
	defer clear(pt[:cap(pt)])
	var total int64
	for i := range r.n {
		p, _, err := r.readChunk(i, pt, ct)
		if err != nil {
			return total, err
		}
		n, err := w.Write(p)
		total += int64(n)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
