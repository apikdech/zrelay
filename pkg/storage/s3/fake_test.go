package s3_test

import (
	"bytes"
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
)

// fakeS3 is a minimal path-style S3 stub: enough of HeadBucket, objects,
// ListObjectsV2 and multipart uploads for the driver's unit tests. Keys are
// stored without the bucket.
type fakeS3 struct {
	URL string

	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]*fakeUpload
	nextID  int
	calls   []string // "<METHOD> <op>" in arrival order

	// fail, when set, answers every request with this status and error code
	// (no body for HEAD).
	failStatus int
	failCode   string
	// dropComplete is the number of CompleteMultipartUpload requests whose
	// response is lost: the upload completes, then the connection is cut
	// before any response byte is written.
	dropComplete int
	// failHead is the number of HeadObject requests answered with
	// headStatus/headCode before HEAD works again.
	failHead   int
	headStatus int
	headCode   string
}

type fakeUpload struct {
	key   string
	parts map[int][]byte
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, uploads: map[string]*fakeUpload{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func (f *fakeS3) set(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

func (f *fakeS3) get(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

func (f *fakeS3) del(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

func (f *fakeS3) openUploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func (f *fakeS3) dropCompleteResponses(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropComplete = n
}

func (f *fakeS3) failHeadObject(n, status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failHead, f.headStatus, f.headCode = n, status, code
}

func (f *fakeS3) failWith(status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus, f.failCode = status, code
}

func (f *fakeS3) callsOf(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

func writeS3Error(w http.ResponseWriter, r *http.Request, status int, code string) {
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>fake</Message><RequestId>fake</RequestId></Error>`, code)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := r.URL.Query()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	_ = bucket

	op := "object"
	switch {
	case key == "" && q.Get("list-type") == "2":
		op = "list"
	case key == "":
		op = "bucket"
	case q.Has("uploads"):
		op = "create"
	case q.Has("partNumber"):
		op = "part"
	case q.Has("uploadId") && r.Method == http.MethodPost:
		op = "complete"
	case q.Has("uploadId") && r.Method == http.MethodDelete:
		op = "abort"
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+op)

	if f.failStatus != 0 {
		writeS3Error(w, r, f.failStatus, f.failCode)
		return
	}

	if op == "object" && r.Method == http.MethodHead && f.failHead > 0 {
		f.failHead--
		writeS3Error(w, r, f.headStatus, f.headCode)
		return
	}

	switch op {
	case "bucket":
		w.WriteHeader(http.StatusOK)
	case "list":
		f.list(w, q.Get("prefix"))
	case "create":
		f.nextID++
		id := "upload-" + strconv.Itoa(f.nextID)
		f.uploads[id] = &fakeUpload{key: key, parts: map[int][]byte{}}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, bucket, key, id)
	case "part":
		u, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			writeS3Error(w, r, http.StatusNotFound, "NoSuchUpload")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		u.parts[n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d-%d"`, n, len(body)))
		w.WriteHeader(http.StatusOK)
	case "complete":
		id := q.Get("uploadId")
		u, ok := f.uploads[id]
		if !ok {
			writeS3Error(w, r, http.StatusNotFound, "NoSuchUpload")
			return
		}
		var req struct {
			Parts []struct {
				PartNumber int
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			writeS3Error(w, r, http.StatusBadRequest, "MalformedXML")
			return
		}
		var obj bytes.Buffer
		for _, p := range req.Parts {
			obj.Write(u.parts[p.PartNumber])
		}
		f.objects[key] = obj.Bytes()
		delete(f.uploads, id)
		if f.dropComplete > 0 {
			f.dropComplete--
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"multi-%d"</ETag></CompleteMultipartUploadResult>`, bucket, key, len(req.Parts))
	case "abort":
		if _, ok := f.uploads[q.Get("uploadId")]; !ok {
			writeS3Error(w, r, http.StatusNotFound, "NoSuchUpload")
			return
		}
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	default:
		f.object(w, r, key, body)
	}
}

func (f *fakeS3) object(w http.ResponseWriter, r *http.Request, key string, body []byte) {
	switch r.Method {
	case http.MethodPut:
		f.objects[key] = body
		w.Header().Set("ETag", fmt.Sprintf(`"put-%d"`, len(body)))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodHead, http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			writeS3Error(w, r, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", fmt.Sprintf(`"obj-%d"`, len(data)))
		w.Header().Set("Last-Modified", time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	default:
		writeS3Error(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// list answers ListObjectsV2 by byte prefix, in one page.
func (f *fakeS3) list(w http.ResponseWriter, prefix string) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, `<ListBucketResult><Name>b</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`, prefix, len(keys))
	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>2026-11-06T00:00:00.000Z</LastModified><ETag>"obj-%d"</ETag><Size>%d</Size></Contents>`, k, len(f.objects[k]), len(f.objects[k]))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}
