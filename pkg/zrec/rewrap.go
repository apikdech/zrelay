package zrec

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
)

// unwrapDEK looks up h.KeyID in ring and unwraps the clip's DEK, in the
// Contract §4.4 order: ErrUnknownKey, then ErrAuthHeader.
func unwrapDEK(h Header, hd [32]byte, ring Keyring) (dek [32]byte, err error) {
	keys, ok := ring[h.KeyID]
	if !ok {
		return dek, fmt.Errorf("%w: %s", ErrUnknownKey, FormatKeyID(h.KeyID))
	}
	wrapAAD := append(hd[:], h.KeyID[:]...)
	if _, err := newGCM(keys.Wrap[:]).Open(dek[:0], h.WrapNonce[:], h.WrappedDEK[:], wrapAAD); err != nil {
		return dek, ErrAuthHeader
	}
	return dek, nil
}

// Rewrap copies the size-byte clip in r to w with its key block re-sealed
// under to (Contract §3.4): the DEK is unwrapped with the key in from that
// matches the header's key_id and wrapped again under to.Wrap with a fresh
// wrap_nonce read from rnd (nil means crypto/rand). The descriptor and every
// chunk are copied byte for byte, so chunk ciphertext is not decrypted or
// verified; a damaged chunk survives a rotation unchanged. Nothing is
// written to w unless the header is valid and the DEK unwraps.
func Rewrap(r io.ReaderAt, size int64, w io.Writer, from Keyring, to Keys, rnd io.Reader) error {
	if size < 0 {
		return fmt.Errorf("zrec: negative size %d", size)
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	hb := make([]byte, min(size, MaxHeaderLen))
	if err := readFullAt(r, hb, 0); err != nil {
		return fmt.Errorf("zrec: read header: %w", err)
	}
	h, err := ParseHeader(hb)
	if err != nil {
		return err
	}
	hd := sha256.Sum256(h.Descriptor())
	dek, err := unwrapDEK(h, hd, from)
	if err != nil {
		return err
	}
	defer clear(dek[:])

	h.KeyID = to.ID
	if _, err := io.ReadFull(rnd, h.WrapNonce[:]); err != nil {
		return fmt.Errorf("zrec: generate wrap_nonce: %w", err)
	}
	wrapAAD := append(hd[:], h.KeyID[:]...)
	newGCM(to.Wrap[:]).Seal(h.WrappedDEK[:0], h.WrapNonce[:], dek[:], wrapAAD)

	if err := writeFull(w, h.marshal()); err != nil {
		return fmt.Errorf("zrec: write header: %w", err)
	}
	want := size - int64(h.HeaderLen)
	n, err := io.Copy(w, io.NewSectionReader(r, int64(h.HeaderLen), want))
	if err != nil {
		return fmt.Errorf("zrec: copy chunks: %w", err)
	}
	if n != want {
		return fmt.Errorf("zrec: copied %d of %d chunk bytes: %w", n, want, io.ErrUnexpectedEOF)
	}
	return nil
}
