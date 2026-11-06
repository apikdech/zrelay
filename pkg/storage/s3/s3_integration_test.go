//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
	"github.com/apikdech/zrelay-v2/pkg/storage/storagetest"
)

type minioEnv struct {
	endpoint, accessKey, secretKey, bucket string
}

func startMinIO(t *testing.T) minioEnv {
	t.Helper()
	var e minioEnv
	e.endpoint, e.accessKey, e.secretKey, e.bucket = storagetest.StartMinIO(t)
	return e
}

var rootSeq atomic.Int64

// driver returns a driver on a fresh root, so its key space is empty.
func (e minioEnv) driver(t *testing.T, endpoint string) *s3.Driver {
	t.Helper()
	d, err := s3.New(t.Context(), s3.Config{
		Endpoint:     endpoint,
		Region:       "us-east-1",
		Bucket:       e.bucket,
		Root:         fmt.Sprintf("cctv-%d", rootSeq.Add(1)),
		AccessKey:    e.accessKey,
		SecretKey:    e.secretKey,
		UsePathStyle: true,
		PartSize:     5 * mib,
		// A private transport, so no connection is shared across drivers.
		HTTPClient: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestS3Conformance(t *testing.T) {
	e := startMinIO(t)
	storagetest.Run(t, func(t *testing.T) storage.Provider { return e.driver(t, e.endpoint) })
}

func TestS3DurableOffsetIsCompletedParts(t *testing.T) {
	e := startMinIO(t)
	d := e.driver(t, e.endpoint)
	ctx := t.Context()
	w, err := d.Create(ctx, "clips/cam/2026/11/06/120000000_01JC0000000000000000000000.zrec")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = w.Abort(context.Background()) }()
	chunk := bytes.Repeat([]byte{0xa5}, mib)
	for range 12 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := w.DurableOffset(); got != 10*mib {
		t.Fatalf("DurableOffset after 12 MiB with 5 MiB parts = %d, want %d", got, 10*mib)
	}
}

func TestS3ResumeThroughFaultProxy(t *testing.T) {
	e := startMinIO(t)
	proxy := storagetest.StartFaultProxy(t, e.endpoint)
	d := e.driver(t, proxy.URL())
	ctx := t.Context()
	const key = "clips/cam/2026/11/06/120000000_01JC0000000000000000000001.zrec"

	src := make([]byte, 40*mib)
	rng := rand.NewChaCha8([32]byte{18})
	_, _ = rng.Read(src)

	w, err := d.Create(ctx, key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Cut points are stream offsets; the proxy counts bytes sent upstream from
	// the moment it is armed (request headers make the cut land slightly
	// earlier in the stream, which stays within the same part).
	kills := []int64{12 * mib, 27 * mib}
	wantResume := []int64{10 * mib, 25 * mib}
	proxy.KillAfterBytes(kills[0])
	var off int64
	cuts := 0
	for off < int64(len(src)) {
		end := min(off+mib, int64(len(src)))
		if _, err := w.Write(src[off:end]); err != nil {
			if cuts == len(kills) {
				t.Fatalf("unexpected Write error at %d after %d cuts: %v", off, cuts, err)
			}
			durable := w.DurableOffset()
			off, err = w.Resume(ctx)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if off != wantResume[cuts] || off != durable {
				t.Fatalf("Resume after cut %d = %d (DurableOffset %d), want %d", cuts+1, off, durable, wantResume[cuts])
			}
			cuts++
			if cuts < len(kills) {
				proxy.KillAfterBytes(kills[cuts] - off)
			}
			continue
		}
		off = end
	}
	if cuts != len(kills) {
		t.Fatalf("saw %d connection cuts, want %d", cuts, len(kills))
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	or, err := d.OpenRange(ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer or.Close()
	h := sha256.New()
	n, err := io.Copy(h, or)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := sha256.Sum256(src)
	if n != int64(len(src)) || !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatalf("committed object: %d bytes, sha256 %x; want %d bytes, sha256 %x", n, h.Sum(nil), len(src), want)
	}
}
