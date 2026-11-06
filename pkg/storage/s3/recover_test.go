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

// fakeRecoverS3 adds paginated ListMultipartUploads (with MinIO's prefix
// semantics) and ListParts to fakeS3, with a per-upload Initiated time, and
// serves everything else through it.
type fakeRecoverS3 struct {
	*fakeS3
	URL        string
	maxUploads int // page size of ListMultipartUploads
	maxParts   int // page size of ListParts
	initiated  map[string]time.Time
	// uncached holds the uploads created before a simulated server restart.
	// MinIO serves a ListMultipartUploads with an empty prefix from an
	// in-memory cache of uploads started since the process began, so these
	// are no longer listed by it; an exact object name still finds them.
	uncached map[string]bool
	// stuck makes every truncated ListMultipartUploads page repeat the
	// request's markers, as a broken server would.
	stuck bool
}

func newFakeRecoverS3(t *testing.T) *fakeRecoverS3 {
	t.Helper()
	f := &fakeRecoverS3{fakeS3: newFakeS3(t), maxUploads: 2, maxParts: 1, initiated: map[string]time.Time{}, uncached: map[string]bool{}}
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

// restart simulates a server restart: every open upload leaves the cache an
// empty-prefix listing is served from.
func (f *fakeRecoverS3) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.uploads {
		f.uncached[id] = true
	}
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
		// As MinIO does: a non-empty prefix matches only the exact object
		// name, never a byte prefix of it.
		if prefix == "" && f.uncached[id] {
			continue
		}
		if (prefix == "" || u.key == prefix) && (u.key > keyMarker || u.key == keyMarker && id > idMarker) {
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
		if f.stuck {
			last = up{keyMarker, idMarker}
		}
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

func TestListIncompleteStuckListingErrors(t *testing.T) {
	f := newFakeRecoverS3(t)
	f.maxUploads = 1
	old := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)
	f.addUpload("cctv/clips/cam/a.zrec", old, map[int][]byte{})
	f.addUpload("cctv/clips/cam/b.zrec", old, map[int][]byte{})
	f.stuck = true
	d := stubDriver(t, f.URL, "cctv")

	done := make(chan error, 1)
	go func() {
		_, err := d.ListIncomplete(t.Context(), "clips/cam", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not advance") {
			t.Errorf("ListIncomplete with a stuck truncated listing error = %v, want a \"did not advance\" error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListIncomplete looped on a truncated listing that did not advance")
	}
}

func TestListIncompleteSurvivesServerRestart(t *testing.T) {
	const (
		dir  = "clips/cam/2026/11/06/"
		base = "120000000_01JC0000000000000000000019"
		pre  = "cctv/" + dir
	)
	f := newFakeRecoverS3(t)
	old := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)
	parts := map[int][]byte{1: []byte("aaaa"), 2: []byte("bb")}
	f.addUpload(pre+base+".zrec", old, parts)
	f.set(pre+"."+base+".json.part", []byte("{}"))
	// An upload with no staging sidecar: only the cache can find it.
	f.addUpload(pre+"130000000_01JC000000000000000000001A.zrec", old, parts)
	// Staging-like objects that must not produce candidates.
	f.set(pre+".140000000_01JC000000000000000000001B.json.part", []byte("{}")) // no upload
	f.set(pre+".150000000_01JC000000000000000000001C.tmp", []byte("x"))
	f.set(pre+".160000000_01JC000000000000000000001D.json", []byte("x"))
	f.set(pre+"170000000_01JC000000000000000000001E.json.part", []byte("x")) // not a dot-name
	// Another camera, with a sidecar of its own.
	f.addUpload("cctv/clips/cam2/2026/11/06/180000000_01JC000000000000000000001F.zrec", old, parts)
	f.set("cctv/clips/cam2/2026/11/06/.180000000_01JC000000000000000000001F.json.part", []byte("{}"))
	d := stubDriver(t, f.URL, "cctv")

	list := func(prefix string, olderThan time.Duration) []string {
		t.Helper()
		incs, err := d.ListIncomplete(t.Context(), prefix, olderThan)
		if err != nil {
			t.Fatalf("ListIncomplete(%q): %v", prefix, err)
		}
		var ks []string
		for _, inc := range incs {
			if inc.Key == dir+base+".zrec" && (inc.Size != 6 || inc.UploadID == "" || inc.StagingKey != "") {
				t.Errorf("Incomplete %+v: want size 6, an upload id and no staging key", inc)
			}
			ks = append(ks, inc.Key)
		}
		slices.Sort(ks)
		return ks
	}
	withSidecar := dir + base + ".zrec"
	bare := dir + "130000000_01JC000000000000000000001A.zrec"

	if got, want := list("clips/cam", 0), []string{withSidecar, bare}; !slices.Equal(got, want) {
		t.Errorf("before the restart ListIncomplete(clips/cam) = %q, want %q (each upload once)", got, want)
	}
	f.restart()
	if got, want := list("clips/cam", 0), []string{withSidecar}; !slices.Equal(got, want) {
		t.Errorf("after the restart ListIncomplete(clips/cam) = %q, want %q (the upload with a staging sidecar)", got, want)
	}
	if got := list("clips/cam", time.Hour); len(got) != 1 {
		t.Errorf("after the restart ListIncomplete(clips/cam, 1h) = %q, want the old upload", got)
	}
	if got, want := list("", 0), []string{withSidecar, "clips/cam2/2026/11/06/180000000_01JC000000000000000000001F.zrec"}; !slices.Equal(got, want) {
		t.Errorf("after the restart ListIncomplete(\"\") = %q, want %q", got, want)
	}
	if got := list("clips/ca", 0); len(got) != 0 {
		t.Errorf("after the restart ListIncomplete(clips/ca) = %q, want none (whole path elements)", got)
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

func TestListStagingFiltersDotNamedSidecars(t *testing.T) {
	const pre = "cctv/clips/front/2026/11/06/"
	f := newFakeRecoverS3(t)
	f.set(pre+".120000000_01JC0000000000000000000019.json.part", []byte("{}"))
	f.set(pre+".130000000_01JC000000000000000000001A.json.part", []byte("{}"))
	f.set(pre+"140000000_01JC000000000000000000001B.json", []byte("{}"))      // final sidecar
	f.set(pre+"150000000_01JC000000000000000000001C.json.part", []byte("{}")) // not a dot-name
	f.set(pre+".160000000_01JC000000000000000000001D.tmp", []byte("x"))
	f.set("cctv/clips/front-door/2026/11/06/.120000000_01JC000000000000000000001E.json.part", []byte("{}"))
	f.set("other/clips/front/2026/11/06/.120000000_01JC000000000000000000001F.json.part", []byte("{}")) // outside the root
	d := stubDriver(t, f.URL, "cctv")

	keys := func(prefix string) []string {
		t.Helper()
		infos, err := d.ListStaging(t.Context(), prefix)
		if err != nil {
			t.Fatalf("ListStaging(%q): %v", prefix, err)
		}
		var ks []string
		for _, in := range infos {
			if in.Size != 2 || in.ModTime.Location() != time.UTC || in.ModTime.IsZero() {
				t.Errorf("ListStaging(%q) entry %+v: want Size 2 and a UTC ModTime", prefix, in)
			}
			ks = append(ks, in.Key)
		}
		slices.Sort(ks)
		return ks
	}
	mine := []string{
		"clips/front/2026/11/06/.120000000_01JC0000000000000000000019.json.part",
		"clips/front/2026/11/06/.130000000_01JC000000000000000000001A.json.part",
	}
	if got := keys("clips/front"); !slices.Equal(got, mine) {
		t.Errorf("ListStaging(clips/front) = %q, want %q", got, mine)
	}
	if got := keys(""); len(got) != 3 {
		t.Errorf("ListStaging(\"\") = %q, want the 3 staging sidecars under the root", got)
	}
	if got := keys("clips/fro"); len(got) != 0 {
		t.Errorf("ListStaging(clips/fro) = %q, want none (whole path elements)", got)
	}
	if _, err := d.ListStaging(t.Context(), "../x"); !errors.Is(err, storage.ErrInvalidKey) {
		t.Errorf("ListStaging(../x) error = %v, want ErrInvalidKey", err)
	}
}
