package egress

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/recorder"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
)

// stubS3 is a path-style S3 endpoint for the real s3 driver: PutObject,
// DeleteObject, HeadObject and the multipart calls, with the hooks the
// vanished-upload and lost-response scenarios need. The fakeStore of
// egress_test.go cannot stand in for these: it answers NotFound from Resume
// and Commit, which the real driver never does.
type stubS3 struct {
	URL string

	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	keys    map[string]string // upload id -> key
	nextID  int
	calls   []string // "<METHOD> <op>"

	// partGone, when it returns true for (upload id, part number), makes
	// UploadPart answer 404 NoSuchUpload, as when an operator removes the
	// incomplete upload under a live clip.
	partGone func(uploadID string, part int) bool
	// completeGone makes CompleteMultipartUpload answer 404 NoSuchUpload
	// without publishing anything.
	completeGone func(uploadID string) bool
	// dropComplete is the number of CompleteMultipartUpload requests that
	// succeed upstream but whose response is cut before any byte is sent.
	dropComplete int
}

func newStubS3(t *testing.T) *stubS3 {
	t.Helper()
	s := &stubS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, keys: map[string]string{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func (s *stubS3) count(call string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (s *stubS3) object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects["cctv/"+key]
	return bytes.Clone(b), ok
}

func stubError(w http.ResponseWriter, r *http.Request, status int, code string) {
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>stub</Message><RequestId>stub</RequestId></Error>`, code)
}

func (s *stubS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := r.URL.Query()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	op := "object"
	switch {
	case q.Has("uploads"):
		op = "create"
	case q.Has("partNumber"):
		op = "part"
	case q.Has("uploadId") && r.Method == http.MethodPost:
		op = "complete"
	case q.Has("uploadId") && r.Method == http.MethodDelete:
		op = "abort"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+op)

	switch op {
	case "create":
		s.nextID++
		id := "upload-" + strconv.Itoa(s.nextID)
		s.uploads[id] = map[int][]byte{}
		s.keys[id] = key
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, bucket, key, id)
	case "part":
		id := q.Get("uploadId")
		n, _ := strconv.Atoi(q.Get("partNumber"))
		parts, ok := s.uploads[id]
		if !ok || (s.partGone != nil && s.partGone(id, n)) {
			delete(s.uploads, id)
			stubError(w, r, http.StatusNotFound, "NoSuchUpload")
			return
		}
		parts[n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d-%d"`, n, len(body)))
		w.WriteHeader(http.StatusOK)
	case "complete":
		id := q.Get("uploadId")
		parts, ok := s.uploads[id]
		if !ok || (s.completeGone != nil && s.completeGone(id)) {
			delete(s.uploads, id)
			stubError(w, r, http.StatusNotFound, "NoSuchUpload")
			return
		}
		var req struct {
			Parts []struct{ PartNumber int } `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			stubError(w, r, http.StatusBadRequest, "MalformedXML")
			return
		}
		var obj bytes.Buffer
		for _, p := range req.Parts {
			obj.Write(parts[p.PartNumber])
		}
		s.objects[key] = obj.Bytes()
		delete(s.uploads, id)
		if s.dropComplete > 0 {
			s.dropComplete--
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"multi"</ETag></CompleteMultipartUploadResult>`, bucket, key)
	case "abort":
		id := q.Get("uploadId")
		if _, ok := s.uploads[id]; !ok {
			stubError(w, r, http.StatusNotFound, "NoSuchUpload")
			return
		}
		delete(s.uploads, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		switch r.Method {
		case http.MethodPut:
			s.objects[key] = body
			w.Header().Set("ETag", `"put"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(s.objects, key)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodHead, http.MethodGet:
			data, ok := s.objects[key]
			if !ok {
				stubError(w, r, http.StatusNotFound, "NoSuchKey")
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("ETag", `"obj"`)
			w.Header().Set("Last-Modified", time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
		default:
			stubError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
		}
	}
}

// s3Rig is an Egress over the real s3 driver and the stub, with the real
// s3.Classify and a breaker that counts its state changes.
type s3Rig struct {
	t     *testing.T
	e     *Egress
	stub  *stubS3
	clk   *clock.Fake
	br    *Breaker
	mu    sync.Mutex
	drops []drop
	done  []string
}

func newS3Rig(t *testing.T, stub *stubS3) *s3Rig {
	t.Helper()
	d, err := s3.New(t.Context(), s3.Config{
		Endpoint: stub.URL, Region: "us-east-1", Bucket: "bucket", Root: "cctv",
		AccessKey: "AKIDTEST", SecretKey: "secret-test", UsePathStyle: true,
		PartSize: s3.MinPartSize,
	})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	// The stub keys objects as "<bucket>/<key>"; the driver prefixes the root.
	clk := clock.NewFake(time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC))
	r := &s3Rig{t: t, stub: stub, clk: clk}
	r.br = NewBreaker(BreakerConfig{FailureThreshold: 5, BaseDelay: time.Second, MaxDelay: time.Minute}, clk,
		func(context.Context) error { return nil }, nil)
	r.e = New(Config{
		Storage: d, Workers: 1, BacklogLimit: 1 << 30, Breaker: r.br, Clock: clk,
		Classify: s3.Classify,
		OnDrop: func(camera, reason string, ms int64) {
			r.mu.Lock()
			r.drops = append(r.drops, drop{camera, reason, ms})
			r.mu.Unlock()
		},
		OnCommitted: func(camera string, at time.Time) {
			r.mu.Lock()
			r.done = append(r.done, camera)
			r.mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { r.e.Run(ctx); close(runDone) }()
	t.Cleanup(func() { cancel(); <-runDone })
	return r
}

// begin registers a clip for camera "front" and returns it with its writer.
func (r *s3Rig) begin(id string) *testClip {
	r.t.Helper()
	dir := "clips/front/2026/11/06"
	pc := &recorder.PendingClip{
		Keys: sidecar.ClipKeySet{
			Dir:            dir,
			Video:          dir + "/145900000_" + id + ".zrec",
			Sidecar:        dir + "/145900000_" + id + ".json",
			StagingSidecar: dir + "/.145900000_" + id + ".json.part",
		},
		Staging: []byte(`{"staging":"` + id + `"}`),
		Final:   func() ([]byte, error) { return []byte(`{"final":"` + id + `"}`), nil },
	}
	w, err := r.e.Begin(pc)
	if err != nil {
		r.t.Fatalf("Begin: %v", err)
	}
	return &testClip{pc: pc, w: w}
}

func (r *s3Rig) dropList() []drop {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.drops)
}
