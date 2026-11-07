package zrec_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// seqReader yields an incrementing byte pattern, so Rewrap's wrap_nonce is
// predictable.
type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

func testKeys(seed byte) zrec.Keys {
	var m zrec.MasterKey
	for i := range m {
		m[i] = seed + byte(i)
	}
	return zrec.Derive(m)
}

// sealClip writes a clip of n pseudo-random-ish plaintext bytes under k with
// 64 KiB chunks.
func sealClip(t *testing.T, k zrec.Keys, n int) (clip, plain []byte) {
	t.Helper()
	const startMs = int64(1789570740000)
	id, err := ulid.New(uint64(startMs), &seqReader{n: 7})
	if err != nil {
		t.Fatal(err)
	}
	plain = make([]byte, n)
	for i := range plain {
		plain[i] = byte(i * 31)
	}
	var buf bytes.Buffer
	w, err := zrec.NewWriter(&buf, zrec.WriterParams{
		Keys: k, ClipID: id, StartUnixMs: startMs, CameraID: "front-door",
		ChunkSize: 65536, Rand: &seqReader{n: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), plain
}

func TestRewrapLeavesChunksIdentical(t *testing.T) {
	from, to := testKeys(0), testKeys(50)
	clip, plain := sealClip(t, from, 5*65536/2)

	hdr, err := zrec.ParseHeader(clip)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = zrec.Rewrap(bytes.NewReader(clip), int64(len(clip)), &out,
		zrec.Keyring{from.ID: from}, to, &seqReader{n: 200})
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	got := out.Bytes()
	if len(got) != len(clip) {
		t.Fatalf("rewrapped length %d, want %d", len(got), len(clip))
	}
	hl := int(hdr.HeaderLen)
	if !bytes.Equal(got[hl:], clip[hl:]) {
		t.Fatal("bytes after header_len changed")
	}
	// The descriptor is untouched; only the key block (key_id, nonce, DEK) differs.
	if !bytes.Equal(got[:len(hdr.Descriptor())], clip[:len(hdr.Descriptor())]) {
		t.Fatal("descriptor changed")
	}
	nh, err := zrec.ParseHeader(got)
	if err != nil {
		t.Fatal(err)
	}
	if nh.KeyID != to.ID || nh.KeyID == hdr.KeyID {
		t.Fatalf("key_id = %x, want %x", nh.KeyID, to.ID)
	}
	if nh.WrapNonce == hdr.WrapNonce || nh.WrappedDEK == hdr.WrappedDEK {
		t.Fatal("key block was not re-sealed with a fresh nonce")
	}

	// Opens with the new key only, and yields the same plaintext.
	r, err := zrec.OpenReader(bytes.NewReader(got), int64(len(got)), zrec.Keyring{to.ID: to}, zrec.ReaderOptions{})
	if err != nil {
		t.Fatalf("OpenReader(to): %v", err)
	}
	var dec bytes.Buffer
	if _, err := r.WriteTo(&dec); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !bytes.Equal(dec.Bytes(), plain) {
		t.Fatal("plaintext differs after rewrap")
	}
	if _, err := zrec.OpenReader(bytes.NewReader(got), int64(len(got)), zrec.Keyring{from.ID: from}, zrec.ReaderOptions{}); !errors.Is(err, zrec.ErrUnknownKey) {
		t.Fatalf("OpenReader(from) = %v, want ErrUnknownKey", err)
	}
}

func TestRewrapDoesNotTouchDamagedChunks(t *testing.T) {
	from, to := testKeys(0), testKeys(50)
	clip, _ := sealClip(t, from, 3*65536)
	hdr, _ := zrec.ParseHeader(clip)
	clip[int(hdr.HeaderLen)+70000] ^= 0x01 // corrupt chunk 1

	var out bytes.Buffer
	if err := zrec.Rewrap(bytes.NewReader(clip), int64(len(clip)), &out,
		zrec.Keyring{from.ID: from}, to, &seqReader{}); err != nil {
		t.Fatalf("Rewrap of a clip with a damaged chunk: %v", err)
	}
	if !bytes.Equal(out.Bytes()[hdr.HeaderLen:], clip[hdr.HeaderLen:]) {
		t.Fatal("chunk bytes changed")
	}
}

// oversizedHeader is a clip whose camera_id_len (100) exceeds 64 while
// header_len (205) is consistent with it.
func oversizedHeader(t *testing.T) []byte {
	t.Helper()
	clip, _ := sealClip(t, testKeys(0), 1000)
	b := bytes.Clone(clip)
	b[36] = 100
	binary.BigEndian.PutUint16(b[6:8], 105+100)
	return b
}

func TestRewrapErrors(t *testing.T) {
	from, to := testKeys(0), testKeys(50)
	clip, _ := sealClip(t, from, 1000)
	hdr, _ := zrec.ParseHeader(clip)

	cases := []struct {
		name string
		clip []byte
		ring zrec.Keyring
		rnd  io.Reader
		want error
	}{
		{"unknown from key", clip, zrec.Keyring{to.ID: to}, &seqReader{}, zrec.ErrUnknownKey},
		{"empty keyring", clip, zrec.Keyring{}, &seqReader{}, zrec.ErrUnknownKey},
		{"bad format", append([]byte("XXXX"), clip[4:]...), zrec.Keyring{from.ID: from}, &seqReader{}, zrec.ErrFormat},
		{"camera_id_len 100", oversizedHeader(t), zrec.Keyring{from.ID: from}, &seqReader{}, zrec.ErrFormat},
		{"truncated header", clip[:hdr.HeaderLen-1], zrec.Keyring{from.ID: from}, &seqReader{}, zrec.ErrTruncated},
		{"wrapped DEK damaged", func() []byte {
			b := bytes.Clone(clip)
			b[hdr.HeaderLen-1] ^= 0x01
			return b
		}(), zrec.Keyring{from.ID: from}, &seqReader{}, zrec.ErrAuthHeader},
		{"rand fails", clip, zrec.Keyring{from.ID: from}, bytes.NewReader(nil), io.EOF},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := zrec.Rewrap(bytes.NewReader(c.clip), int64(len(c.clip)), &out, c.ring, to, c.rnd)
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote %d bytes before failing", out.Len())
			}
		})
	}
}

func TestRewrapRejectsNegativeSize(t *testing.T) {
	from, to := testKeys(0), testKeys(50)
	clip, _ := sealClip(t, from, 1000)
	var out bytes.Buffer
	err := zrec.Rewrap(bytes.NewReader(clip), -1, &out, zrec.Keyring{from.ID: from}, to, &seqReader{})
	if err == nil {
		t.Fatal("negative size accepted")
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %d bytes before failing", out.Len())
	}
}
