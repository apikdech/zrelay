//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

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
	return e.driverOn(t, endpoint, fmt.Sprintf("cctv-%d", rootSeq.Add(1)))
}

// driverOn returns a driver on the given root, e.g. to reach the same key
// space through a restarted server's new endpoint.
func (e minioEnv) driverOn(t *testing.T, endpoint, root string) *s3.Driver {
	t.Helper()
	d, err := s3.New(t.Context(), s3.Config{
		Endpoint:     endpoint,
		Region:       "us-east-1",
		Bucket:       e.bucket,
		Root:         root,
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

// crashS3 abandons a writer as kill -9 would: the s3 writer holds no
// resources, so dropping it leaves the multipart upload and its completed
// parts on the server, and the buffered tail part is lost.
func crashS3(*testing.T, storage.ObjectWriter) {}

// crashedUpload creates key, writes data in 1 MiB slices and crashes the
// writer, returning the DurableOffset seen just before the crash.
func crashedUpload(t *testing.T, d *s3.Driver, key string, data []byte) int64 {
	t.Helper()
	w, err := d.Create(t.Context(), key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for off := 0; off < len(data); off += mib {
		if _, err := w.Write(data[off:min(off+mib, len(data))]); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	durable := w.DurableOffset()
	crashS3(t, w)
	return durable
}

func findUpload(t *testing.T, d *s3.Driver, prefix string, olderThan time.Duration, key string) (storage.Incomplete, bool) {
	t.Helper()
	incs, err := d.ListIncomplete(t.Context(), prefix, olderThan)
	if err != nil {
		t.Fatalf("ListIncomplete(%q, %v): %v", prefix, olderThan, err)
	}
	for _, inc := range incs {
		if inc.Key == key {
			return inc, true
		}
	}
	return storage.Incomplete{}, false
}

func TestS3Recoverer(t *testing.T) {
	e := startMinIO(t)
	storagetest.RunRecoverer(t,
		func(t *testing.T) (storage.Provider, storage.Recoverer) {
			d := e.driver(t, e.endpoint)
			return d, d
		},
		crashS3)

	// The suite's crash leaves 2 full parts and a partial buffer; on s3 the
	// recovered object is exactly those 2 parts (Contract §7.4, accepted loss).
	t.Run("RecoversExactlyCompletedParts", func(t *testing.T) {
		d := e.driver(t, e.endpoint)
		const key = "clips/cam/2026/11/06/120000000_01JC0000000000000000000019.zrec"
		data := make([]byte, 11*mib+123)
		rng := rand.NewChaCha8([32]byte{19})
		_, _ = rng.Read(data)

		if durable := crashedUpload(t, d, key, data); durable != 10*mib {
			t.Fatalf("DurableOffset before the crash = %d, want %d (2 parts)", durable, 10*mib)
		}
		inc, ok := findUpload(t, d, "clips/cam", 0, key)
		if !ok {
			t.Fatalf("ListIncomplete(olderThan 0) does not list %q", key)
		}
		if inc.UploadID == "" || inc.StagingKey != "" {
			t.Errorf("Incomplete UploadID = %q, StagingKey = %q; want a set upload id and no staging key", inc.UploadID, inc.StagingKey)
		}
		if inc.Size != 10*mib {
			t.Errorf("Incomplete.Size = %d, want %d (sum of listed parts)", inc.Size, 10*mib)
		}
		info, err := d.RecoverCommit(t.Context(), inc)
		if err != nil {
			t.Fatalf("RecoverCommit: %v", err)
		}
		if info.Key != key || info.Size != 10*mib || info.ETag == "" {
			t.Fatalf("RecoverCommit = %+v, want key %q, %d bytes and an ETag", info, key, 10*mib)
		}
		or, err := d.OpenRange(t.Context(), key, 0, -1)
		if err != nil {
			t.Fatalf("OpenRange: %v", err)
		}
		defer or.Close()
		got, err := io.ReadAll(or)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !bytes.Equal(got, data[:10*mib]) {
			t.Errorf("recovered object (%d bytes) is not the first 2 parts", len(got))
		}
	})
}

func TestRecoverZeroPartsAborts(t *testing.T) {
	e := startMinIO(t)
	d := e.driver(t, e.endpoint)
	const key = "clips/cam/2026/11/06/120000000_01JC000000000000000000001A.zrec"

	// 3 MiB stays in the part buffer: the upload exists with no parts.
	if durable := crashedUpload(t, d, key, bytes.Repeat([]byte{0x5a}, 3*mib)); durable != 0 {
		t.Fatalf("DurableOffset = %d, want 0", durable)
	}
	inc, ok := findUpload(t, d, "clips/cam", 0, key)
	if !ok {
		t.Fatalf("ListIncomplete does not list %q", key)
	}
	if inc.Size != 0 {
		t.Errorf("Incomplete.Size = %d, want 0", inc.Size)
	}
	if _, err := d.RecoverCommit(t.Context(), inc); !errors.Is(err, storage.ErrNothingToRecover) {
		t.Fatalf("RecoverCommit(zero parts) error = %v, want ErrNothingToRecover", err)
	}
	if _, ok := findUpload(t, d, "clips/cam", 0, key); ok {
		t.Error("the zero-part upload is still listed after RecoverCommit; want it aborted")
	}
	if _, err := d.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after RecoverCommit(zero parts) error = %v, want ErrNotFound", err)
	}
}

func TestListIncompleteRespectsAge(t *testing.T) {
	e := startMinIO(t)
	d := e.driver(t, e.endpoint)
	const key = "clips/cam/2026/11/06/120000000_01JC000000000000000000001B.zrec"
	crashedUpload(t, d, key, bytes.Repeat([]byte{0x3c}, 6*mib))

	inc, ok := findUpload(t, d, "clips/cam", 0, key)
	if !ok {
		t.Fatalf("ListIncomplete(olderThan 0) does not list %q", key)
	}
	if inc.ModTime.IsZero() || inc.ModTime.Location() != time.UTC {
		t.Errorf("Incomplete.ModTime = %v, want the non-zero UTC Initiated time", inc.ModTime)
	}
	if age := time.Since(inc.ModTime); age > time.Minute || age < -time.Minute {
		t.Errorf("Incomplete.ModTime = %v is %v from now, want the upload's Initiated time", inc.ModTime, age)
	}
	if inc, ok := findUpload(t, d, "clips/cam", time.Hour, key); ok {
		t.Errorf("ListIncomplete(olderThan 1h) listed a fresh upload: %+v", inc)
	}
}

// TestListIncompleteSurvivesMinIORestart is the power-cut case: the server
// restarts after the relay crashed mid-upload. MinIO then lists no uploads
// for an empty prefix (it serves that from a cache of the uploads started
// since the process began), so recovery finds the candidate through its
// staging sidecar and an exact-key lookup (design §6.5, Contract §7.4).
func TestListIncompleteSurvivesMinIORestart(t *testing.T) {
	m := storagetest.StartRestartableMinIO(t)
	e := minioEnv{endpoint: m.Endpoint, accessKey: m.AccessKey, secretKey: m.SecretKey, bucket: m.Bucket}
	const (
		root = "cctv-restart"
		dir  = "clips/cam/2026/11/06/"
		base = "120000000_01JC000000000000000000001D"
		key  = dir + base + ".zrec"
	)
	d := e.driverOn(t, m.Endpoint, root)
	data := make([]byte, 11*mib+123)
	rng := rand.NewChaCha8([32]byte{20})
	_, _ = rng.Read(data)

	// Contract §7.3 step 0: the staging sidecar precedes the first chunk.
	if err := d.PutSmall(t.Context(), dir+"."+base+".json.part", []byte(`{"schema":"zrec.sidecar/2"}`), "application/json"); err != nil {
		t.Fatalf("PutSmall(staging sidecar): %v", err)
	}
	if durable := crashedUpload(t, d, key, data); durable != 10*mib {
		t.Fatalf("DurableOffset before the crash = %d, want %d (2 parts)", durable, 10*mib)
	}
	if _, ok := findUpload(t, d, "clips/cam", 0, key); !ok {
		t.Fatalf("ListIncomplete does not list %q before the restart", key)
	}

	m.Restart(t)
	d = e.driverOn(t, m.Endpoint, root)

	inc, ok := findUpload(t, d, "clips/cam", 0, key)
	if !ok {
		t.Fatalf("ListIncomplete does not list %q after the MinIO restart", key)
	}
	if inc.UploadID == "" || inc.Size != 10*mib {
		t.Errorf("Incomplete = %+v, want an upload id and %d bytes", inc, 10*mib)
	}
	info, err := d.RecoverCommit(t.Context(), inc)
	if err != nil {
		t.Fatalf("RecoverCommit after the restart: %v", err)
	}
	if info.Key != key || info.Size != 10*mib {
		t.Fatalf("RecoverCommit = %+v, want key %q and %d bytes", info, key, 10*mib)
	}
	or, err := d.OpenRange(t.Context(), key, 0, -1)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer or.Close()
	got, err := io.ReadAll(or)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, data[:10*mib]) {
		t.Errorf("recovered object (%d bytes) is not the first 2 parts", len(got))
	}
}

func TestAbortIncompleteRemovesUpload(t *testing.T) {
	e := startMinIO(t)
	d := e.driver(t, e.endpoint)
	const key = "clips/cam/2026/11/06/120000000_01JC000000000000000000001C.zrec"
	crashedUpload(t, d, key, bytes.Repeat([]byte{0x7e}, 6*mib))

	inc, ok := findUpload(t, d, "clips/cam", 0, key)
	if !ok {
		t.Fatalf("ListIncomplete does not list %q", key)
	}
	if err := d.AbortIncomplete(t.Context(), inc); err != nil {
		t.Fatalf("AbortIncomplete: %v", err)
	}
	if _, ok := findUpload(t, d, "clips/cam", 0, key); ok {
		t.Error("the upload is still listed after AbortIncomplete")
	}
	if _, err := d.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after AbortIncomplete error = %v, want ErrNotFound", err)
	}
	if err := d.AbortIncomplete(t.Context(), inc); err != nil {
		t.Errorf("second AbortIncomplete error = %v, want nil (idempotent)", err)
	}
}
