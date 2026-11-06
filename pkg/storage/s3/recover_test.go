package s3_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
)

// fakeRecoverS3 adds paginated ListMultipartUploads and ListParts to fakeS3,
// with a per-upload Initiated time, and serves everything else through it.
type fakeRecoverS3 struct {
	*fakeS3
	URL        string
	maxUploads int // page size of ListMultipartUploads
	maxParts   int // page size of ListParts
	initiated  map[string]time.Time
}

func newFakeRecoverS3(t *testing.T) *fakeRecoverS3 {
	t.Helper()
	f := &fakeRecoverS3{fakeS3: newFakeS3(t), maxUploads: 2, maxParts: 1, initiated: map[string]time.Time{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// addUpload stages an upload of key (bucket-relative) with the given parts,
// initiated at the given time, and returns its id.
func (f *fakeRecoverS3) addUpload(key string, initiated time.Time, parts map[int][]byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("upload-%03d", f.nextID)
	f.uploads[id] = &fakeUpload{key: key, parts: parts}
	f.initiated[id] = initiated
	return id
}

func (f *fakeRecoverS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && key == "" && q.Has("uploads"):
		f.listUploads(w, q.Get("prefix"), q.Get("key-marker"), q.Get("upload-id-marker"))
	case r.Method == http.MethodGet && key != "" && q.Has("uploadId"):
		f.listParts(w, r, q.Get("uploadId"), q.Get("part-number-marker"))
	default:
		f.fakeS3.ServeHTTP(w, r)
	}
}

func (f *fakeRecoverS3) listUploads(w http.ResponseWriter, prefix, keyMarker, idMarker string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type up struct{ key, id string }
	var all []up
	for id, u := range f.uploads {
		if strings.HasPrefix(u.key, prefix) && (u.key > keyMarker || u.key == keyMarker && id > idMarker) {
			all = append(all, up{u.key, id})
		}
	}
	slices.SortFunc(all, func(a, b up) int { return strings.Compare(a.key+"\x00"+a.id, b.key+"\x00"+b.id) })
	truncated := len(all) > f.maxUploads
	if truncated {
		all = all[:f.maxUploads]
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<ListMultipartUploadsResult><Bucket>zrelay</Bucket><Prefix>%s</Prefix><MaxUploads>%d</MaxUploads><IsTruncated>%t</IsTruncated>`, prefix, f.maxUploads, truncated)
	if truncated {
		last := all[len(all)-1]
		fmt.Fprintf(&b, `<NextKeyMarker>%s</NextKeyMarker><NextUploadIdMarker>%s</NextUploadIdMarker>`, last.key, last.id)
	}
	for _, u := range all {
		init := f.initiated[u.id]
		if init.IsZero() {
			init = time.Now()
		}
		fmt.Fprintf(&b, `<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>`, u.key, u.id, init.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	b.WriteString(`</ListMultipartUploadsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func (f *fakeRecoverS3) listParts(w http.ResponseWriter, r *http.Request, id, marker string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "GET parts")
	u, ok := f.uploads[id]
	if !ok {
		writeS3Error(w, r, http.StatusNotFound, "NoSuchUpload")
		return
	}
	after, _ := strconv.Atoi(marker)
	var nums []int
	for n := range u.parts {
		if n > after {
			nums = append(nums, n)
		}
	}
	slices.Sort(nums)
	truncated := len(nums) > f.maxParts
	if truncated {
		nums = nums[:f.maxParts]
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<ListPartsResult><Bucket>zrelay</Bucket><Key>%s</Key><UploadId>%s</UploadId><MaxParts>%d</MaxParts><IsTruncated>%t</IsTruncated>`, u.key, id, f.maxParts, truncated)
	if truncated {
		fmt.Fprintf(&b, `<NextPartNumberMarker>%d</NextPartNumberMarker>`, nums[len(nums)-1])
	}
	for _, n := range nums {
		fmt.Fprintf(&b, `<Part><PartNumber>%d</PartNumber><ETag>"part-%d-%d"</ETag><Size>%d</Size><LastModified>2026-11-06T00:00:00.000Z</LastModified></Part>`, n, n, len(u.parts[n]), len(u.parts[n]))
	}
	b.WriteString(`</ListPartsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

const recKey = "clips/front/2026/11/06/120000000_01JC0000000000000000000019.zrec"

func TestListIncompletePaginatesAndFilters(t *testing.T) {
	f := newFakeRecoverS3(t)
	old := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)
	f.addUpload("cctv/"+recKey, old, map[int][]byte{1: []byte("aaaa"), 2: []byte("bb"), 3: []byte("c")})
	f.addUpload("cctv/"+recKey, old, map[int][]byte{}) // a second upload of the same key
	f.addUpload("cctv/clips/front-door/2026/11/06/x.zrec", old, map[int][]byte{1: []byte("x")})
	f.addUpload("cctv/clips/frontx", old, map[int][]byte{1: []byte("x")})
	f.addUpload("other/"+recKey, old, map[int][]byte{1: []byte("x")})
	f.addUpload("cctv/clips/front/2026/11/07/y.zrec", old, map[int][]byte{1: []byte("yy")})
	d := stubDriver(t, f.URL, "cctv")

	incs, err := d.ListIncomplete(t.Context(), "clips/front", time.Hour)
	if err != nil {
		t.Fatalf("ListIncomplete: %v", err)
	}
	var got []string
	for _, inc := range incs {
		if inc.UploadID == "" || inc.StagingKey != "" || !inc.ModTime.Equal(old) || inc.ModTime.Location() != time.UTC {
			t.Errorf("Incomplete %+v: want an upload id, no staging key and ModTime %v (UTC)", inc, old)
		}
		got = append(got, fmt.Sprintf("%s=%d", inc.Key, inc.Size))
	}
	slices.Sort(got)
	want := []string{
		"clips/front/2026/11/06/120000000_01JC0000000000000000000019.zrec=0",
		"clips/front/2026/11/06/120000000_01JC0000000000000000000019.zrec=7",
		"clips/front/2026/11/07/y.zrec=2",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListIncomplete(clips/front) = %q, want %q", got, want)
	}

	all, err := d.ListIncomplete(t.Context(), "", time.Hour)
	if err != nil {
		t.Fatalf("ListIncomplete(all): %v", err)
	}
	if len(all) != 5 {
		t.Errorf("ListIncomplete(\"\") listed %d uploads, want the 5 under the root: %+v", len(all), all)
	}
	if _, err := d.ListIncomplete(t.Context(), "../x", 0); !errors.Is(err, storage.ErrInvalidKey) {
		t.Errorf("ListIncomplete(../x) error = %v, want ErrInvalidKey", err)
	}
}

func TestListIncompleteFiltersByInitiated(t *testing.T) {
	f := newFakeRecoverS3(t)
	f.addUpload("cctv/clips/cam/old.zrec", time.Now().Add(-2*time.Hour), map[int][]byte{})
	f.addUpload("cctv/clips/cam/new.zrec", time.Now(), map[int][]byte{})
	// Server clock ahead of ours: still listed with olderThan 0.
	f.addUpload("cctv/clips/cam/skew.zrec", time.Now().Add(time.Minute), map[int][]byte{})
	d := stubDriver(t, f.URL, "cctv")

	keys := func(olderThan time.Duration) []string {
		incs, err := d.ListIncomplete(t.Context(), "clips/cam", olderThan)
		if err != nil {
			t.Fatalf("ListIncomplete: %v", err)
		}
		var ks []string
		for _, inc := range incs {
			ks = append(ks, inc.Key)
		}
		slices.Sort(ks)
		return ks
	}
	if got, want := keys(time.Hour), []string{"clips/cam/old.zrec"}; !slices.Equal(got, want) {
		t.Errorf("olderThan 1h = %q, want %q", got, want)
	}
	if got, want := keys(0), []string{"clips/cam/new.zrec", "clips/cam/old.zrec", "clips/cam/skew.zrec"}; !slices.Equal(got, want) {
		t.Errorf("olderThan 0 = %q, want %q", got, want)
	}
}

func TestRecoverCommitCompletesListedParts(t *testing.T) {
	f := newFakeRecoverS3(t)
	f.addUpload("cctv/"+recKey, time.Now(), map[int][]byte{1: []byte("first-"), 2: []byte("second-"), 4: []byte("fourth")})
	d := stubDriver(t, f.URL, "cctv")
	incs, err := d.ListIncomplete(t.Context(), "clips", 0)
	if err != nil || len(incs) != 1 {
		t.Fatalf("ListIncomplete = %+v, %v; want one upload", incs, err)
	}

	info, err := d.RecoverCommit(t.Context(), incs[0])
	if err != nil {
		t.Fatalf("RecoverCommit: %v", err)
	}
	want := []byte("first-second-fourth")
	if info.Key != recKey || info.Size != int64(len(want)) || info.ETag == "" {
		t.Errorf("RecoverCommit = %+v, want key %q, size %d and an ETag", info, recKey, len(want))
	}
	if got := f.get("cctv/" + recKey); !bytes.Equal(got, want) {
		t.Errorf("committed object = %q, want %q", got, want)
	}
	if n := f.openUploads(); n != 0 {
		t.Errorf("open uploads after RecoverCommit = %d, want 0", n)
	}
}

func TestRecoverCommitRetryAfterLostResponse(t *testing.T) {
	f := newFakeRecoverS3(t)
	f.addUpload("cctv/"+recKey, time.Now(), map[int][]byte{1: []byte("abc"), 2: []byte("de")})
	d := stubDriver(t, f.URL, "cctv")
	incs, err := d.ListIncomplete(t.Context(), "clips", 0)
	if err != nil || len(incs) != 1 {
		t.Fatalf("ListIncomplete = %+v, %v; want one upload", incs, err)
	}
	f.dropCompleteResponses(1)
	if _, err := d.RecoverCommit(t.Context(), incs[0]); err == nil {
		t.Fatal("RecoverCommit with a lost response error = nil, want an error")
	} else if c := s3.Classify(err); c != storage.Transient {
		t.Errorf("Classify(lost response) = %d, want Transient", c)
	}
	info, err := d.RecoverCommit(t.Context(), incs[0])
	if err != nil {
		t.Fatalf("retried RecoverCommit (upload already completed) error = %v, want nil", err)
	}
	if info.Size != 5 || info.Key != recKey {
		t.Errorf("retried RecoverCommit = %+v, want %q with 5 bytes", info, recKey)
	}
}

func TestRecoverCommitGoneUpload(t *testing.T) {
	f := newFakeRecoverS3(t)
	d := stubDriver(t, f.URL, "cctv")
	inc := storage.Incomplete{Key: recKey, UploadID: "upload-gone", Size: 5}

	// Neither upload nor object: nothing was published.
	if _, err := d.RecoverCommit(t.Context(), inc); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("RecoverCommit(gone, no object) error = %v, want ErrNotFound", err)
	}
	// An object of another size is not this upload's.
	f.set("cctv/"+recKey, []byte("wrong-size"))
	if _, err := d.RecoverCommit(t.Context(), inc); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("RecoverCommit(gone, other object) error = %v, want ErrNotFound", err)
	}
}

func TestRecoverCommitZeroPartsAbortsUpload(t *testing.T) {
	f := newFakeRecoverS3(t)
	id := f.addUpload("cctv/"+recKey, time.Now(), map[int][]byte{})
	d := stubDriver(t, f.URL, "cctv")
	_, err := d.RecoverCommit(t.Context(), storage.Incomplete{Key: recKey, UploadID: id})
	if !errors.Is(err, storage.ErrNothingToRecover) {
		t.Fatalf("RecoverCommit(zero parts) error = %v, want ErrNothingToRecover", err)
	}
	if n := f.openUploads(); n != 0 {
		t.Errorf("open uploads = %d, want 0 (aborted)", n)
	}
	if f.get("cctv/"+recKey) != nil {
		t.Error("RecoverCommit(zero parts) published an object")
	}
}

func TestRecoverCommitRejectsBadCandidate(t *testing.T) {
	f := newFakeRecoverS3(t)
	d := stubDriver(t, f.URL, "cctv")
	for _, inc := range []storage.Incomplete{
		{Key: recKey},                                        // no upload id
		{Key: "../x", UploadID: "upload-1"},                  // bad key
		{Key: recKey, UploadID: "upload-1", StagingKey: "x"}, // a filesystem candidate
	} {
		if _, err := d.RecoverCommit(t.Context(), inc); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("RecoverCommit(%+v) error = %v, want ErrInvalidKey", inc, err)
		}
		if err := d.AbortIncomplete(t.Context(), inc); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("AbortIncomplete(%+v) error = %v, want ErrInvalidKey", inc, err)
		}
	}
	if n := len(f.calls); n != 0 {
		t.Errorf("bad candidates sent %d requests, want 0", n)
	}
}

func TestAbortIncompleteIsIdempotent(t *testing.T) {
	f := newFakeRecoverS3(t)
	id := f.addUpload("cctv/"+recKey, time.Now(), map[int][]byte{1: []byte("x")})
	d := stubDriver(t, f.URL, "cctv")
	inc := storage.Incomplete{Key: recKey, UploadID: id}
	if err := d.AbortIncomplete(t.Context(), inc); err != nil {
		t.Fatalf("AbortIncomplete: %v", err)
	}
	if n := f.openUploads(); n != 0 {
		t.Errorf("open uploads = %d, want 0", n)
	}
	if err := d.AbortIncomplete(t.Context(), inc); err != nil {
		t.Errorf("second AbortIncomplete error = %v, want nil", err)
	}
}
