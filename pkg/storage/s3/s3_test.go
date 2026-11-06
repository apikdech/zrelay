package s3_test

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
)

const mib = 1 << 20

func stubDriver(t *testing.T, endpoint, root string) *s3.Driver {
	t.Helper()
	d, err := s3.New(t.Context(), s3.Config{
		Endpoint:     endpoint,
		Region:       "us-east-1",
		Bucket:       "zrelay",
		Root:         root,
		AccessKey:    "AKIDTEST",
		SecretKey:    "secret-test",
		UsePathStyle: true,
		PartSize:     5 * mib,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestListElementwiseFilter(t *testing.T) {
	f := newFakeS3(t)
	for _, k := range []string{
		"cctv/clips/front-door/x",
		"cctv/clips/frontx",
		"cctv/clips/front/a.json",
		"cctv/clips/front/2026/11/06/b.json",
		"cctv/clips/front/.c.json.part",
		"other/clips/front/z.json",
	} {
		f.set(k, []byte("x"))
	}
	d := stubDriver(t, f.URL, "cctv")

	var got []string
	for info, err := range d.List(t.Context(), "clips/front") {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if info.Size != 1 || info.ETag == "" || info.ModTime.IsZero() {
			t.Errorf("List entry %+v lacks metadata", info)
		}
		got = append(got, info.Key)
	}
	slices.Sort(got)
	want := []string{"clips/front/2026/11/06/b.json", "clips/front/a.json"}
	if !slices.Equal(got, want) {
		t.Errorf("List(clips/front) = %q, want %q", got, want)
	}
}

func TestCommitRetryAfterLostResponse(t *testing.T) {
	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000000.zrec"
	data := bytes.Repeat([]byte("zrec"), 6*mib/4) // one full part + 1 MiB

	t.Run("ObjectPresentIsCommitted", func(t *testing.T) {
		f := newFakeS3(t)
		f.dropCompleteResponses(1)
		d := stubDriver(t, f.URL, "cctv")
		ctx := t.Context()
		w, err := d.Create(ctx, key)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("Write: %v", err)
		}
		err = w.Commit(ctx)
		if err == nil {
			t.Fatal("Commit with a lost response error = nil, want an error")
		}
		if c := s3.Classify(err); c != storage.Transient {
			t.Errorf("Classify(lost response) = %d, want Transient", c)
		}
		if err := w.Commit(ctx); err != nil {
			t.Fatalf("retried Commit (upload already completed) error = %v, want nil", err)
		}
		if n := f.callsOf("POST complete"); n != 2 {
			t.Errorf("CompleteMultipartUpload calls = %d, want 2", n)
		}
		if n := f.callsOf("HEAD object"); n != 1 {
			t.Errorf("HeadObject calls = %d, want 1", n)
		}
		info, err := d.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != int64(len(data)) {
			t.Errorf("Size = %d, want %d", info.Size, len(data))
		}
		if !bytes.Equal(f.get("cctv/"+key), data) {
			t.Error("committed object content differs")
		}
		if _, err := w.Write([]byte("late")); err == nil {
			t.Error("Write after Commit error = nil, want error")
		}
	})

	t.Run("ObjectMissingIsError", func(t *testing.T) {
		f := newFakeS3(t)
		f.dropCompleteResponses(1)
		d := stubDriver(t, f.URL, "cctv")
		ctx := t.Context()
		w, err := d.Create(ctx, key)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Commit(ctx); err == nil {
			t.Fatal("Commit with a lost response error = nil, want an error")
		}
		f.del("cctv/" + key)
		err = w.Commit(ctx)
		if err == nil {
			t.Fatal("retried Commit with NoSuchUpload and no object error = nil, want error")
		}
		if c := s3.Classify(err); c != storage.Permanent {
			t.Errorf("Classify(upload gone, object missing) = %d, want Permanent", c)
		}
	})

	t.Run("ObjectWrongSizeIsError", func(t *testing.T) {
		f := newFakeS3(t)
		f.dropCompleteResponses(1)
		d := stubDriver(t, f.URL, "cctv")
		ctx := t.Context()
		w, err := d.Create(ctx, key)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Commit(ctx); err == nil {
			t.Fatal("Commit with a lost response error = nil, want an error")
		}
		f.set("cctv/"+key, data[:10])
		err = w.Commit(ctx)
		if err == nil {
			t.Fatal("retried Commit with NoSuchUpload and a short object error = nil, want error")
		}
		if c := s3.Classify(err); c != storage.Permanent {
			t.Errorf("Classify(upload gone, wrong size) = %d, want Permanent", c)
		}
	})

	// lostCommit writes data and runs one Commit whose
	// CompleteMultipartUpload succeeds upstream but loses its response.
	lostCommit := func(t *testing.T, f *fakeS3, d *s3.Driver) storage.ObjectWriter {
		t.Helper()
		f.dropCompleteResponses(1)
		w, err := d.Create(t.Context(), key)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Commit(t.Context()); err == nil {
			t.Fatal("Commit with a lost response error = nil, want an error")
		}
		return w
	}

	t.Run("HeadUnavailableIsTransient", func(t *testing.T) {
		f := newFakeS3(t)
		d := stubDriver(t, f.URL, "cctv")
		w := lostCommit(t, f, d)
		f.failHeadObject(1, http.StatusServiceUnavailable, "ServiceUnavailable")
		err := w.Commit(t.Context())
		if err == nil {
			t.Fatal("retried Commit with HEAD 503 error = nil, want error")
		}
		if c := s3.Classify(err); c != storage.Transient {
			t.Errorf("Classify(upload gone, HEAD 503) = %d, want Transient: %v", c, err)
		}
	})

	t.Run("HeadRecoversThenCommitted", func(t *testing.T) {
		f := newFakeS3(t)
		d := stubDriver(t, f.URL, "cctv")
		w := lostCommit(t, f, d)
		f.failHeadObject(1, http.StatusServiceUnavailable, "ServiceUnavailable")
		if err := w.Commit(t.Context()); err == nil {
			t.Fatal("retried Commit with HEAD 503 error = nil, want error")
		}
		if err := w.Commit(t.Context()); err != nil {
			t.Fatalf("Commit after HEAD recovered error = %v, want nil", err)
		}
		if n := f.callsOf("HEAD object"); n != 2 {
			t.Errorf("HeadObject calls = %d, want 2", n)
		}
		if !bytes.Equal(f.get("cctv/"+key), data) {
			t.Error("committed object content differs")
		}
	})

	t.Run("WriteRefusedOnceCompleteStarted", func(t *testing.T) {
		f := newFakeS3(t)
		d := stubDriver(t, f.URL, "cctv")
		w := lostCommit(t, f, d)
		parts := f.callsOf("PUT part")
		if _, err := w.Write(bytes.Repeat([]byte("x"), 5*mib)); err == nil {
			t.Error("Write after CompleteMultipartUpload started error = nil, want error")
		} else if c := s3.Classify(err); c != storage.Permanent {
			t.Errorf("Classify(Write after Complete started) = %d, want Permanent", c)
		}
		off, err := w.Resume(t.Context())
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if off != int64(len(data)) {
			t.Errorf("Resume = %d, want %d", off, len(data))
		}
		if _, err := w.Write([]byte("x")); err == nil {
			t.Error("Write after Resume past a started Complete error = nil, want error")
		}
		if n := f.callsOf("PUT part"); n != parts {
			t.Errorf("UploadPart calls after Complete started = %d, want 0", n-parts)
		}
		if err := w.Commit(t.Context()); err != nil {
			t.Fatalf("retried Commit: %v", err)
		}
		if !bytes.Equal(f.get("cctv/"+key), data) {
			t.Error("committed object content differs")
		}
	})

	t.Run("AbortAfterLostResponseReportsCommitted", func(t *testing.T) {
		f := newFakeS3(t)
		d := stubDriver(t, f.URL, "cctv")
		w := lostCommit(t, f, d)
		if err := w.Abort(t.Context()); err == nil {
			t.Error("Abort of an upload that was already completed error = nil, want error")
		} else if c := s3.Classify(err); c != storage.Permanent {
			t.Errorf("Classify(Abort of a published object) = %d, want Permanent", c)
		}
		if n := f.callsOf("HEAD object"); n != 1 {
			t.Errorf("HeadObject calls = %d, want 1", n)
		}
		if !bytes.Equal(f.get("cctv/"+key), data) {
			t.Error("object published by the lost Complete was changed")
		}
	})
}

func TestCommitZeroBytesIsError(t *testing.T) {
	f := newFakeS3(t)
	d := stubDriver(t, f.URL, "")
	ctx := t.Context()
	w, err := d.Create(ctx, "clips/cam/empty.zrec")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.Commit(ctx); err == nil {
		t.Fatal("zero-byte Commit error = nil, want error")
	}
	if n := f.callsOf("POST complete"); n != 0 {
		t.Errorf("CompleteMultipartUpload calls = %d, want 0", n)
	}
	if err := w.Abort(ctx); err != nil {
		t.Fatalf("Abort after failed zero-byte Commit: %v", err)
	}
	if n := f.openUploads(); n != 0 {
		t.Errorf("uploads left after Abort: %d", n)
	}
}

func TestResumeDiscardsInFlightPart(t *testing.T) {
	f := newFakeS3(t)
	d := stubDriver(t, f.URL, "")
	ctx := t.Context()
	const key = "clips/cam/x.zrec"
	w, err := d.Create(ctx, key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	src := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 2*mib)
	if _, err := w.Write(src[:7*mib]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := w.DurableOffset(); got != 5*mib {
		t.Fatalf("DurableOffset = %d, want %d", got, 5*mib)
	}
	off, err := w.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if off != 5*mib {
		t.Fatalf("Resume = %d, want %d (end of the last completed part)", off, 5*mib)
	}
	if _, err := w.Write(src[off:]); err != nil {
		t.Fatalf("Write after Resume: %v", err)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := f.get(key); !bytes.Equal(got, src) {
		t.Fatalf("object after Resume differs (%d bytes, want %d)", len(got), len(src))
	}
}

// A transient failure of the final (short) part inside Commit must not wedge
// the writer: the retried Commit re-sends the same part and completes, with
// no Resume and no re-Write.
func TestCommitRetryAfterFinalPartFailure(t *testing.T) {
	f := newFakeS3(t)
	d := stubDriver(t, f.URL, "")
	ctx := t.Context()
	const key = "clips/cam/tail.zrec"
	w, err := d.Create(ctx, key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	src := bytes.Repeat([]byte{9, 8, 7, 6, 5, 4, 3}, 5*mib/7+1000)[:5*mib+1000]
	if _, err := w.Write(src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.failPartUpload(2, 1)
	err = w.Commit(ctx)
	if err == nil {
		t.Fatal("Commit with a failing final part: error = nil")
	}
	if got := s3.Classify(err); got != storage.Transient {
		t.Fatalf("Classify(final part failure) = %v, want Transient (%v)", got, err)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("retried Commit: %v", err)
	}
	if got := f.get(key); !bytes.Equal(got, src) {
		t.Fatalf("object differs (%d bytes, want %d)", len(got), len(src))
	}
}

// After a failed final part, Resume discards the tail and reports the
// durable offset; re-sending the tail and committing yields the full object.
func TestResumeAfterFinalPartFailureThenResendTail(t *testing.T) {
	f := newFakeS3(t)
	d := stubDriver(t, f.URL, "")
	ctx := t.Context()
	const key = "clips/cam/tail2.zrec"
	w, err := d.Create(ctx, key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	src := bytes.Repeat([]byte{1, 3, 5, 7, 11}, 5*mib/5+200)[:5*mib+1000]
	if _, err := w.Write(src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.failPartUpload(2, 1)
	if err := w.Commit(ctx); err == nil {
		t.Fatal("Commit error = nil, want the part failure")
	}
	off, err := w.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if off != 5*mib {
		t.Fatalf("Resume = %d, want %d", off, 5*mib)
	}
	if _, err := w.Write(src[off:]); err != nil {
		t.Fatalf("Write tail: %v", err)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := f.get(key); !bytes.Equal(got, src) {
		t.Fatalf("object differs (%d bytes, want %d)", len(got), len(src))
	}
}

// errNeedsResume is writer state, not a storage verdict: it must never reach
// the breaker as a Permanent failure.
func TestNeedsResumeIsNotPermanent(t *testing.T) {
	f := newFakeS3(t)
	d := stubDriver(t, f.URL, "")
	ctx := t.Context()
	w, err := d.Create(ctx, "clips/cam/nr.zrec")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.failPartUpload(1, 1)
	if _, err := w.Write(make([]byte, 5*mib)); err == nil {
		t.Fatal("Write with a failing part: error = nil")
	}
	_, err = w.Write([]byte("x"))
	if err == nil {
		t.Fatal("Write before Resume: error = nil")
	}
	if got := s3.Classify(err); got == storage.Permanent {
		t.Errorf("Classify(%v) = Permanent, want not Permanent", err)
	}
	err = w.Commit(ctx)
	if err == nil {
		t.Fatal("Commit before Resume: error = nil")
	}
	if got := s3.Classify(err); got == storage.Permanent {
		t.Errorf("Classify(%v) = Permanent, want not Permanent", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	good := s3.Config{
		Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Bucket: "zrelay",
		AccessKey: "a", SecretKey: "s", UsePathStyle: true,
	}
	if _, err := s3.New(t.Context(), good); err != nil {
		t.Fatalf("New(valid, default PartSize): %v", err)
	}
	for name, mut := range map[string]func(*s3.Config){
		"NoBucket":         func(c *s3.Config) { c.Bucket = "" },
		"NoAccessKey":      func(c *s3.Config) { c.AccessKey = "" },
		"NoSecretKey":      func(c *s3.Config) { c.SecretKey = "" },
		"PartSizeTooSmall": func(c *s3.Config) { c.PartSize = 5*mib - 1 },
		"BadRoot":          func(c *s3.Config) { c.Root = "../x" },
		"EndpointUserinfo": func(c *s3.Config) { c.Endpoint = "http://user:secret-pw@127.0.0.1:1" },
		"EndpointNoScheme": func(c *s3.Config) { c.Endpoint = "127.0.0.1:9000" },
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			mut(&c)
			_, err := s3.New(t.Context(), c)
			if err == nil {
				t.Fatal("New error = nil, want error")
			}
			if strings.Contains(err.Error(), "secret-") {
				t.Errorf("error leaks the secret key: %v", err)
			}
		})
	}
}

func TestStatMissingIsNotFound(t *testing.T) {
	f := newFakeS3(t)
	d := stubDriver(t, f.URL, "cctv")
	if _, err := d.Stat(t.Context(), "clips/none.json"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := d.OpenRange(t.Context(), "clips/none.json", 0, -1); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("OpenRange(missing) error = %v, want ErrNotFound", err)
	}
}
