package zrec

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// TestRoundTripProperty writes and reads back clips of 300 random sizes in
// [1, 5·65536] plus every exact multiple 1..5 × 65536, fed to the writer in
// random piece sizes, and checks the plaintext, chunk count and random
// access to one chunk.
func TestRoundTripProperty(t *testing.T) {
	const cs = 1 << 16
	rng := rand.New(rand.NewPCG(0x7a726563, 1)) // fixed seed: failures reproduce
	sizes := make([]int, 0, 305)
	for range 300 {
		sizes = append(sizes, 1+rng.IntN(5*cs))
	}
	for k := 1; k <= 5; k++ {
		sizes = append(sizes, k*cs)
	}

	src := make([]byte, 5*cs)
	for i := range src {
		src[i] = byte(rng.Uint32())
	}
	ring := testRing()

	for _, size := range sizes {
		plain := src[:size]
		p := testParams(t, cs)
		p.Rand = nil // fresh DEK and nonce from crypto/rand
		piece := 1 + rng.IntN(3*cs)
		_, file := writeClip(t, p, plain, piece)

		r, err := OpenReader(bytes.NewReader(file), int64(len(file)), ring, ReaderOptions{})
		if err != nil {
			t.Fatalf("size %d: OpenReader: %v", size, err)
		}
		wantN := (size + cs - 1) / cs
		if r.NumChunks() != wantN {
			t.Fatalf("size %d: NumChunks = %d, want %d", size, r.NumChunks(), wantN)
		}
		var out bytes.Buffer
		n, err := r.WriteTo(&out)
		if err != nil {
			t.Fatalf("size %d (piece %d): WriteTo: %v", size, piece, err)
		}
		if n != int64(size) || !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("size %d (piece %d): round trip mismatch (n=%d)", size, piece, n)
		}

		i := rng.IntN(wantN)
		got, final, err := r.ReadChunk(i, nil)
		if err != nil {
			t.Fatalf("size %d: ReadChunk(%d): %v", size, i, err)
		}
		if final != (i == wantN-1) || !bytes.Equal(got, plain[i*cs:min((i+1)*cs, size)]) {
			t.Fatalf("size %d: ReadChunk(%d) mismatch (final=%v)", size, i, final)
		}
	}
}
