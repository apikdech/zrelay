package zrec_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/apikdech/zrelay-v2/pkg/zrec"
	"github.com/apikdech/zrelay-v2/pkg/zrec/internal/vectorgen"
)

const vectorDir = "../../testdata/zrec/v1"

// manifest mirrors the Contract §4.5 vectors.json shape.
type manifest struct {
	FormatVersion int    `json:"format_version"`
	Suite         int    `json:"suite"`
	MasterKeyHex  string `json:"master_key_hex"`
	ChunkSize     int    `json:"chunk_size"`
	CameraID      string `json:"camera_id"`
	ClipID        string `json:"clip_id"`
	StartUnixMs   int64  `json:"start_unix_ms"`
	Derived       struct {
		KeyIDHex      string `json:"key_id_hex"`
		KWrapHex      string `json:"k_wrap_hex"`
		KAPIHex       string `json:"k_api_hex"`
		KAPISHA256Hex string `json:"k_api_sha256_hex"`
	} `json:"derived"`
	Positive []struct {
		Name               string `json:"name"`
		File               string `json:"file"`
		PlaintextLen       int    `json:"plaintext_len"`
		PlaintextSHA256Hex string `json:"plaintext_sha256_hex"`
	} `json:"positive"`
	Negative []struct {
		Name       string `json:"name"`
		File       string `json:"file"`
		Expect     string `json:"expect"`
		ChunkIndex *int   `json:"chunk_index"`
	} `json:"negative"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorDir, "vectors.json"))
	if err != nil {
		t.Fatalf("read manifest (run `make vectors`): %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

var sentinels = map[string]error{
	"ErrFormat":       zrec.ErrFormat,
	"ErrUnknownKey":   zrec.ErrUnknownKey,
	"ErrAuthHeader":   zrec.ErrAuthHeader,
	"ErrAuth":         zrec.ErrAuth,
	"ErrTruncated":    zrec.ErrTruncated,
	"ErrTrailingData": zrec.ErrTrailingData,
}

func ip(i int) *int { return &i }

// The Contract §4.5 case tables, as name → plaintext length and
// name → (expect, chunk_index).
var (
	wantPositive = map[string]int{
		"one-byte":            1,
		"exact-chunk":         65536,
		"two-chunks":          2 * 65536,
		"two-and-half-chunks": 5 * 65536 / 2,
	}
	wantNegative = map[string]struct {
		expect string
		chunk  *int
	}{
		"camera-id-bitflip":      {"ErrAuthHeader", nil},
		"key-id-bitflip":         {"ErrUnknownKey", nil},
		"wrapped-dek-bitflip":    {"ErrAuthHeader", nil},
		"bitflip-chunk":          {"ErrAuth", ip(1)},
		"swapped-chunks":         {"ErrAuth", ip(0)},
		"dropped-final-chunk":    {"ErrTruncated", nil},
		"extra-chunk":            {"ErrTrailingData", nil},
		"transplanted-key-block": {"ErrAuthHeader", nil},
		"wrong-master-key":       {"ErrUnknownKey", nil},
		"trailing-bytes":         {"ErrAuth", ip(2)},
		"header-only":            {"ErrTruncated", nil},
	}
)

func TestVectors(t *testing.T) {
	m := loadManifest(t)

	// Fixed inputs (Contract §4.5).
	var master zrec.MasterKey
	for i := range master {
		master[i] = byte(i)
	}
	if m.FormatVersion != 1 || m.Suite != 1 || m.ChunkSize != 65536 || m.CameraID != "front-door" ||
		m.ClipID != "01M2NBMVS0K8EKRQKGGAJCDT0B" || m.StartUnixMs != 1789570740000 ||
		m.MasterKeyHex != hex.EncodeToString(master[:]) {
		t.Fatalf("manifest inputs differ from Contract §4.5: %+v", m)
	}

	// Derived-value checks.
	k := zrec.Derive(master)
	apiHash := zrec.APITokenHash(k)
	for _, c := range []struct{ name, got, want string }{
		{"key_id_hex", m.Derived.KeyIDHex, hex.EncodeToString(k.ID[:])},
		{"k_wrap_hex", m.Derived.KWrapHex, hex.EncodeToString(k.Wrap[:])},
		{"k_api_hex", m.Derived.KAPIHex, hex.EncodeToString(k.API[:])},
		{"k_api_sha256_hex", m.Derived.KAPISHA256Hex, hex.EncodeToString(apiHash[:])},
	} {
		if c.got != c.want {
			t.Errorf("derived.%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	ring := zrec.Keyring{k.ID: k}

	open := func(t *testing.T, file []byte) (*zrec.Reader, error) {
		return zrec.OpenReader(bytes.NewReader(file), int64(len(file)), ring, zrec.ReaderOptions{})
	}

	var gotPos []string
	for _, v := range m.Positive {
		gotPos = append(gotPos, v.Name)
		t.Run("positive/"+v.Name, func(t *testing.T) {
			wantLen, ok := wantPositive[v.Name]
			if !ok {
				t.Fatalf("unexpected positive vector %q", v.Name)
			}
			if v.File != "pos-"+v.Name+".zrec" || v.PlaintextLen != wantLen {
				t.Fatalf("vector %+v: want file pos-%s.zrec and plaintext_len %d", v, v.Name, wantLen)
			}
			r, err := open(t, readVector(t, v.File))
			if err != nil {
				t.Fatalf("OpenReader: %v", err)
			}
			h := r.Header()
			if h.CameraID != m.CameraID || h.ClipID.String() != m.ClipID || h.StartUnixMs != m.StartUnixMs ||
				int(h.ChunkSize) != m.ChunkSize || h.KeyID != k.ID {
				t.Fatalf("header %+v does not match the manifest", h)
			}
			if want := (wantLen + m.ChunkSize - 1) / m.ChunkSize; r.NumChunks() != want {
				t.Fatalf("NumChunks = %d, want %d", r.NumChunks(), want)
			}
			sum := sha256.New()
			n, err := r.WriteTo(sum)
			if err != nil {
				t.Fatalf("WriteTo: %v", err)
			}
			if n != int64(v.PlaintextLen) || hex.EncodeToString(sum.Sum(nil)) != v.PlaintextSHA256Hex {
				t.Fatalf("plaintext: n=%d sha256=%x, want n=%d sha256=%s", n, sum.Sum(nil), v.PlaintextLen, v.PlaintextSHA256Hex)
			}
		})
	}

	var gotNeg []string
	for _, v := range m.Negative {
		gotNeg = append(gotNeg, v.Name)
		t.Run("negative/"+v.Name, func(t *testing.T) {
			want, ok := wantNegative[v.Name]
			if !ok {
				t.Fatalf("unexpected negative vector %q", v.Name)
			}
			if v.File != "neg-"+v.Name+".zrec" || v.Expect != want.expect || !equalPtr(v.ChunkIndex, want.chunk) {
				t.Fatalf("vector %+v: want file neg-%s.zrec, expect %s, chunk_index %v", v, v.Name, want.expect, want.chunk)
			}
			sentinel := sentinels[v.Expect]
			r, err := open(t, readVector(t, v.File))
			if err == nil {
				_, err = r.WriteTo(&bytes.Buffer{})
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("error = %v, want %s", err, v.Expect)
			}
			for name, other := range sentinels {
				if name != v.Expect && errors.Is(err, other) {
					t.Fatalf("error = %v also matches %s", err, name)
				}
			}
			if v.Expect == "ErrAuth" {
				var ae *zrec.AuthError
				if !errors.As(err, &ae) || ae.Chunk != *v.ChunkIndex {
					t.Fatalf("error = %v, want ErrAuth(%d)", err, *v.ChunkIndex)
				}
			}
		})
	}

	if !sameSet(gotPos, keys(wantPositive)) {
		t.Errorf("positive vectors = %v, want %v", gotPos, keys(wantPositive))
	}
	if !sameSet(gotNeg, keys(wantNegative)) {
		t.Errorf("negative vectors = %v, want %v", gotNeg, keys(wantNegative))
	}
}

// TestVectorsUpToDate regenerates the vectors in memory and byte-compares
// them with the committed files, so the committed set cannot drift from
// the generator.
func TestVectorsUpToDate(t *testing.T) {
	gen, err := vectorgen.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, ok := gen["vectors.json"]; !ok {
		t.Fatal("Generate produced no vectors.json")
	}
	for name, want := range gen {
		got, err := os.ReadFile(filepath.Join(vectorDir, name))
		if err != nil {
			t.Errorf("%s: %v (run `make vectors`)", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the generator output (run `make vectors`)", name)
		}
	}
	entries, err := os.ReadDir(vectorDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := gen[e.Name()]; !ok {
			t.Errorf("stale file %s is not produced by the generator", e.Name())
		}
	}
}

func equalPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
