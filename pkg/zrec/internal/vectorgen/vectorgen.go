// Package vectorgen builds the deterministic zrec format_version 1 test
// vectors of Contract §4.5: the vectors.json manifest and every positive
// and negative .zrec file it lists. cmd genvectors writes them to
// testdata/zrec/v1, and the zrec tests regenerate them in memory to check
// that the committed copies are current.
//
// Fixed inputs: master key 00..1f, chunk_size 65536, camera_id
// "front-door", clip_id 01M2NBMVS0K8EKRQKGGAJCDT0B, start_unix_ms
// 1789570740000, DEK 20..3f and wrap_nonce 40..4b. The plaintext of length
// L is the first L bytes of the SHA-256 counter stream
// SHA-256(uint64_be(0)) || SHA-256(uint64_be(1)) || ….
package vectorgen

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// Fixed vector inputs (Contract §4.5).
const (
	ChunkSize   = 65536
	CameraID    = "front-door"
	ClipID      = "01M2NBMVS0K8EKRQKGGAJCDT0B"
	StartUnixMs = int64(1789570740000)
)

// Manifest is the vectors.json shape of Contract §4.5. Field order is the
// serialization order.
type Manifest struct {
	FormatVersion int     `json:"format_version"`
	Suite         int     `json:"suite"`
	MasterKeyHex  string  `json:"master_key_hex"`
	ChunkSize     int     `json:"chunk_size"`
	CameraID      string  `json:"camera_id"`
	ClipID        string  `json:"clip_id"`
	StartUnixMs   int64   `json:"start_unix_ms"`
	Derived       Derived `json:"derived"`
	Positive      []Pos   `json:"positive"`
	Negative      []Neg   `json:"negative"`
}

// Derived holds the derived-value checks.
type Derived struct {
	KeyIDHex      string `json:"key_id_hex"`
	KWrapHex      string `json:"k_wrap_hex"`
	KAPIHex       string `json:"k_api_hex"`
	KAPISHA256Hex string `json:"k_api_sha256_hex"`
}

// Pos is a positive case.
type Pos struct {
	Name               string `json:"name"`
	File               string `json:"file"`
	PlaintextLen       int    `json:"plaintext_len"`
	PlaintextSHA256Hex string `json:"plaintext_sha256_hex"`
}

// Neg is a negative case; ChunkIndex is set only when Expect is ErrAuth.
type Neg struct {
	Name       string `json:"name"`
	File       string `json:"file"`
	Expect     string `json:"expect"`
	ChunkIndex *int   `json:"chunk_index,omitempty"`
}

// seq returns n bytes start, start+1, ….
func seq(start byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

// Plaintext returns the first n bytes of the SHA-256 counter stream.
func Plaintext(n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	var ctr [8]byte
	for i := uint64(0); len(out) < n; i++ {
		binary.BigEndian.PutUint64(ctr[:], i)
		sum := sha256.Sum256(ctr[:])
		out = append(out, sum[:]...)
	}
	return out[:n]
}

// clip holds the inputs of one encrypted clip.
type clip struct {
	master zrec.MasterKey
	clipID ulid.ULID
	dek    [32]byte
	nonce  []byte
}

func vectorClip() clip {
	var c clip
	copy(c.master[:], seq(0x00, 32))
	c.clipID = ulid.MustParse(ClipID)
	copy(c.dek[:], seq(0x20, 32))
	c.nonce = seq(0x40, 12)
	return c
}

func (c clip) encrypt(plain []byte) ([]byte, error) {
	var out bytes.Buffer
	dek := c.dek
	w, err := zrec.NewWriter(&out, zrec.WriterParams{
		Keys:        zrec.Derive(c.master),
		ClipID:      c.clipID,
		StartUnixMs: StartUnixMs,
		CameraID:    CameraID,
		ChunkSize:   ChunkSize,
		Rand:        bytes.NewReader(c.nonce),
		DEK:         &dek,
	})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func ip(i int) *int { return &i }

// Generate returns every vector file keyed by file name, including
// vectors.json. The output is byte-for-byte deterministic.
func Generate() (map[string][]byte, error) {
	c := vectorClip()
	k := zrec.Derive(c.master)
	apiHash := zrec.APITokenHash(k)
	m := Manifest{
		FormatVersion: 1,
		Suite:         1,
		MasterKeyHex:  hex.EncodeToString(c.master[:]),
		ChunkSize:     ChunkSize,
		CameraID:      CameraID,
		ClipID:        ClipID,
		StartUnixMs:   StartUnixMs,
		Derived: Derived{
			KeyIDHex:      hex.EncodeToString(k.ID[:]),
			KWrapHex:      hex.EncodeToString(k.Wrap[:]),
			KAPIHex:       hex.EncodeToString(k.API[:]),
			KAPISHA256Hex: hex.EncodeToString(apiHash[:]),
		},
	}
	files := map[string][]byte{}

	// Positive cases.
	pos := map[string][]byte{}
	for _, p := range []struct {
		name string
		n    int
	}{
		{"one-byte", 1},
		{"exact-chunk", ChunkSize},
		{"two-chunks", 2 * ChunkSize},
		{"two-and-half-chunks", 5 * ChunkSize / 2},
	} {
		plain := Plaintext(p.n)
		f, err := c.encrypt(plain)
		if err != nil {
			return nil, fmt.Errorf("vectorgen: %s: %w", p.name, err)
		}
		pos[p.name] = f
		sum := sha256.Sum256(plain)
		file := "pos-" + p.name + ".zrec"
		files[file] = f
		m.Positive = append(m.Positive, Pos{p.name, file, p.n, hex.EncodeToString(sum[:])})
	}

	// Negative cases. three is the 2.5-chunk clip (chunks 0 and 1 full,
	// chunk 2 half); two is the 2-chunk clip whose final chunk is full.
	three, two := pos["two-and-half-chunks"], pos["two-chunks"]
	h, err := zrec.ParseHeader(three)
	if err != nil {
		return nil, fmt.Errorf("vectorgen: %w", err)
	}
	hl := int(h.HeaderLen)
	const s = ChunkSize + 16
	chunk := func(f []byte, i int) []byte { return f[hl+i*s : min(hl+(i+1)*s, len(f))] }
	flip := func(f []byte, off int) []byte {
		b := bytes.Clone(f)
		b[off] ^= 0x01
		return b
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	// The donor clip has another clip_id (same timestamp), DEK and nonce.
	donor := c
	donor.clipID[15] ^= 0x01
	copy(donor.dek[:], seq(0x50, 32))
	donor.nonce = seq(0x70, 12)
	donorFile, err := donor.encrypt(Plaintext(1))
	if err != nil {
		return nil, fmt.Errorf("vectorgen: donor: %w", err)
	}
	other := c
	copy(other.master[:], seq(0xe0, 32))
	otherFile, err := other.encrypt(Plaintext(1))
	if err != nil {
		return nil, fmt.Errorf("vectorgen: wrong master: %w", err)
	}

	descLen := hl - 68
	for _, n := range []struct {
		name   string
		file   []byte
		expect string
		chunk  *int
	}{
		// 'f' -> 'g': still a valid camera_id, so the descriptor hash changes.
		{"camera-id-bitflip", flip(three, 37), "ErrAuthHeader", nil},
		{"key-id-bitflip", flip(three, descLen), "ErrUnknownKey", nil},
		{"wrapped-dek-bitflip", flip(three, descLen+20), "ErrAuthHeader", nil},
		{"bitflip-chunk", flip(three, hl+s+s/2), "ErrAuth", ip(1)},
		{"swapped-chunks", cat(three[:hl], chunk(three, 1), chunk(three, 0), chunk(three, 2)), "ErrAuth", ip(0)},
		{"dropped-final-chunk", three[:hl+2*s], "ErrTruncated", nil},
		{"extra-chunk", cat(two, chunk(two, 0)), "ErrTrailingData", nil},
		{"transplanted-key-block", cat(three[:descLen], donorFile[descLen:hl], three[hl:]), "ErrAuthHeader", nil},
		{"wrong-master-key", otherFile, "ErrUnknownKey", nil},
		{"trailing-bytes", cat(three, bytes.Repeat([]byte{0xa5}, 100)), "ErrAuth", ip(2)},
		{"header-only", three[:hl], "ErrTruncated", nil},
	} {
		file := "neg-" + n.name + ".zrec"
		files[file] = n.file
		m.Negative = append(m.Negative, Neg{n.name, file, n.expect, n.chunk})
	}

	js, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	files["vectors.json"] = append(js, '\n')
	return files, nil
}
