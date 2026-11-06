package zview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/sidecar"
)

func TestStatusPassesThroughStorageObject(t *testing.T) {
	e := newEnv(t)
	e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})
	st := sidecar.Status{
		Schema: sidecar.StatusSchema, RelayID: "home-mini", Version: "2.0.0",
		UpdatedAt: sidecar.Time{Time: testNow}, KeyID: "a1b2c3d4e5f60718",
		Cameras: []sidecar.CameraStatus{camStatus("front", "up")},
		Storage: &sidecar.StorageStatus{State: "open", Since: sidecar.Time{Time: testNow}, BacklogBytes: 1234},
	}
	body, _ := json.Marshal(st)
	if err := e.f.drv.PutSmall(context.Background(), sidecar.StatusKey, body, "application/json"); err != nil {
		t.Fatal(err)
	}

	rec := e.do("GET", "/api/v1/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got map[string]json.RawMessage
	decodeJSON(t, rec, &got)
	var storageObj sidecar.StorageStatus
	if err := json.Unmarshal(got["storage"], &storageObj); err != nil || storageObj.State != "open" || storageObj.BacklogBytes != 1234 {
		t.Errorf("storage = %s (%v), want the relay's storage object", got["storage"], err)
	}
	for _, k := range []string{"schema", "relay_id", "updated_at", "cameras", "indexed_days", "invalid_sidecars", "index_fresh_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status lacks %q: %s", k, rec.Body)
		}
	}
}

func TestStatusWithoutRelayObjectStillReportsIndexStats(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/api/v1/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	decodeJSON(t, rec, &got)
	if _, ok := got["indexed_days"]; !ok {
		t.Errorf("no indexed_days: %s", rec.Body)
	}
	if got["index_fresh_at"] != nil {
		t.Errorf("index_fresh_at = %v before any day was indexed, want null", got["index_fresh_at"])
	}
}

func TestCamerasEndpoint(t *testing.T) {
	e := newEnv(t)
	e.f.writeStatus(testNow, camStatus("front", "up"))
	e.f.addClip(clipOpt{Camera: "front", Start: utc(2026, 11, 6, 9, 0, 0)})

	rec := e.do("GET", "/api/v1/cameras", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var cams []CameraSummary
	decodeJSON(t, rec, &cams)
	if len(cams) != 1 || cams[0].ID != "front" || !cams[0].Online || cams[0].LastClipAt == nil {
		t.Errorf("cameras = %+v", cams)
	}
}

func TestClipsListAndFilters(t *testing.T) {
	e := newEnv(t)
	a := e.f.addClip(clipOpt{Camera: "front", Start: utc(2026, 11, 6, 9, 0, 0), Triggers: []string{"person"}})
	b := e.f.addClip(clipOpt{Camera: "back", Start: utc(2026, 11, 6, 10, 0, 0), Triggers: []string{"motion"}})
	c := e.f.addClip(clipOpt{Camera: "front", Start: utc(2026, 11, 6, 11, 0, 0), Triggers: []string{"vehicle"}})

	list := func(q string) []Summary {
		t.Helper()
		rec := e.do("GET", "/api/v1/clips?"+q, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", q, rec.Code, rec.Body)
		}
		var out []Summary
		decodeJSON(t, rec, &out)
		return out
	}
	win := "from=2026-11-06T00:00:00Z&to=2026-11-07T00:00:00Z"
	got := list(win)
	if !sameIDs(got, a, b, c) {
		t.Fatalf("all = %v", ids(got))
	}
	if got[0].VideoHref != "/api/v1/clips/"+a.ClipID.String()+"/video" {
		t.Errorf("video_href = %q", got[0].VideoHref)
	}
	if !sameIDs(list(win+"&camera=back"), b) {
		t.Error("camera filter")
	}
	if !sameIDs(list(win+"&camera=back&camera=front&trigger=person&trigger=vehicle"), a, c) {
		t.Error("repeatable camera and trigger filters")
	}
	if got := list("from=2026-11-06T09:00:00Z&to=2026-11-06T09:00:10Z"); !sameIDs(got, a) {
		t.Errorf("overlap window = %v", ids(got))
	}
	if got := list("from=2026-11-05T00:00:00Z&to=2026-11-05T01:00:00Z"); got == nil || len(got) != 0 {
		t.Errorf("empty window = %#v, want []", got)
	}
}

func TestClipsRejectBadQueries(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{
		"",
		"from=2026-11-06T00:00:00Z",
		"from=yesterday&to=2026-11-07T00:00:00Z",
		"from=2026-11-07T00:00:00Z&to=2026-11-06T00:00:00Z",
		"from=2026-11-01T00:00:00Z&to=2026-11-10T00:00:00Z", // 9 days
		"from=2026-11-06T00:00:00Z&to=2026-11-07T00:00:00Z&trigger=ufo",
		"from=2026-11-06T00:00:00Z&to=2026-11-07T00:00:00Z&camera=../x",
	} {
		wantProblem(t, e.do("GET", "/api/v1/clips?"+q, ""), http.StatusBadRequest)
	}
	if n := e.cr.listCount(); n != 0 {
		t.Errorf("bad queries listed storage %d times", n)
	}
}

func TestClipDetailIsVerbatimSidecarPlusVideoHref(t *testing.T) {
	e := newEnv(t)
	sc := e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})

	rec := e.do("GET", "/api/v1/clips/"+sc.ClipID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got map[string]json.RawMessage
	decodeJSON(t, rec, &got)
	var href string
	_ = json.Unmarshal(got["video_href"], &href)
	if href != "/api/v1/clips/"+sc.ClipID.String()+"/video" {
		t.Errorf("video_href = %q", href)
	}
	var vid struct {
		SeekIndex [][2]int64 `json:"seek_index"`
		Object    string     `json:"object"`
	}
	if err := json.Unmarshal(got["video"], &vid); err != nil || len(vid.SeekIndex) != 2 || vid.Object != sc.Video.Object {
		t.Errorf("video = %s: the sidecar's seek_index and object must be kept", got["video"])
	}
	// Every field of the sidecar is present, not just the summary's.
	want, _ := json.Marshal(sc)
	var wantMap map[string]json.RawMessage
	_ = json.Unmarshal(want, &wantMap)
	for k, v := range wantMap {
		if !bytes.Equal(compact(t, got[k]), compact(t, v)) {
			t.Errorf("field %q = %s, want %s", k, got[k], v)
		}
	}
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatalf("compact %q: %v", b, err)
	}
	return out.Bytes()
}

func TestVideoWithoutRangeServesWholeObject(t *testing.T) {
	e := newEnv(t)
	sc := e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})
	want := e.videoBytes(sc)

	rec := e.do("GET", "/api/v1/clips/"+sc.ClipID.String()+"/video", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Error("body differs from the stored object")
	}
	h := rec.Header()
	if h.Get("ETag") == "" {
		t.Error("no ETag")
	}
	if got := h.Get("Cache-Control"); got != "private, max-age=86400, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q", got)
	}
	if got := h.Get("Content-Length"); got != fmt.Sprint(len(want)) {
		t.Errorf("Content-Length = %q, want %d", got, len(want))
	}
}

func TestVideoRange(t *testing.T) {
	e := newEnv(t)
	sc := e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})
	full := e.videoBytes(sc)
	total := len(full)
	id := sc.ClipID.String()
	if total < 64 {
		t.Fatalf("fixture video too small: %d", total)
	}

	cases := []struct {
		name, rng  string
		status     int
		lo, hi     int // inclusive; for 206
		contentRng string
	}{
		{"closed", "bytes=10-29", 206, 10, 29, fmt.Sprintf("bytes 10-29/%d", total)},
		{"open-ended", "bytes=40-", 206, 40, total - 1, fmt.Sprintf("bytes 40-%d/%d", total-1, total)},
		{"suffix", "bytes=-5", 206, total - 5, total - 1, fmt.Sprintf("bytes %d-%d/%d", total-5, total-1, total)},
		{"end clamped", fmt.Sprintf("bytes=0-%d", total+1000), 206, 0, total - 1, fmt.Sprintf("bytes 0-%d/%d", total-1, total)},
		{"past the end", fmt.Sprintf("bytes=%d-", total), 416, 0, 0, fmt.Sprintf("bytes */%d", total)},
		{"inverted", "bytes=20-10", 416, 0, 0, fmt.Sprintf("bytes */%d", total)},
	}
	for _, c := range cases {
		rec := e.do("GET", "/api/v1/clips/"+id+"/video", "", "Range", c.rng)
		if rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.status)
			continue
		}
		if got := rec.Header().Get("Content-Range"); got != c.contentRng {
			t.Errorf("%s: Content-Range = %q, want %q", c.name, got, c.contentRng)
		}
		if c.status == 206 {
			if !bytes.Equal(rec.Body.Bytes(), full[c.lo:c.hi+1]) {
				t.Errorf("%s: wrong bytes", c.name)
			}
			if rec.Header().Get("ETag") == "" || rec.Header().Get("Cache-Control") != "private, max-age=86400, immutable" {
				t.Errorf("%s: missing ETag or Cache-Control on 206", c.name)
			}
		}
	}

	// Syntax the server does not serve is ignored (RFC 9110 14.2): whole object.
	for _, rng := range []string{"bytes=0-1,5-6", "items=0-1", "bytes=abc"} {
		rec := e.do("GET", "/api/v1/clips/"+id+"/video", "", "Range", rng)
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), full) {
			t.Errorf("Range %q: status %d, want the whole object", rng, rec.Code)
		}
	}
}

func TestVideoConditionalGet(t *testing.T) {
	e := newEnv(t)
	sc := e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})
	target := "/api/v1/clips/" + sc.ClipID.String() + "/video"
	etag := e.do("GET", target, "").Header().Get("ETag")
	rec := e.do("GET", target, "", "If-None-Match", etag)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match = %d with %d body bytes, want 304 and none", rec.Code, rec.Body.Len())
	}
}

func TestVideoMissingObjectIs404(t *testing.T) {
	e := newEnv(t)
	sc := e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})
	// The listing says the video exists; ILM may remove it before the next refresh.
	if rec := e.do("GET", "/api/v1/clips/"+sc.ClipID.String(), ""); rec.Code != 200 {
		t.Fatalf("warm-up = %d", rec.Code)
	}
	e.f.remove(sidecar.ClipKeys(sc.Camera.ID, sc.StartTime.Time, sc.ClipID).Video)
	wantProblem(t, e.do("GET", "/api/v1/clips/"+sc.ClipID.String()+"/video", ""), http.StatusNotFound)
}

func TestClipIDErrors(t *testing.T) {
	e := newEnv(t)
	e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})
	missing := e.f.newID(utc(2026, 11, 6, 9, 30, 0)).String()

	for _, suffix := range []string{"", "/video"} {
		wantProblem(t, e.do("GET", "/api/v1/clips/"+missing+suffix, ""), http.StatusNotFound)
		wantProblem(t, e.do("GET", "/api/v1/clips/not-a-ulid"+suffix, ""), http.StatusBadRequest)
	}
}

func TestClipIDNeverReachesStorageAsAPath(t *testing.T) {
	e := newEnv(t)
	sc := e.f.addClip(clipOpt{Start: utc(2026, 11, 6, 9, 0, 0)})
	// Warm the day so the later lookups can only add OpenRange calls of their own.
	e.do("GET", "/api/v1/clips/"+sc.ClipID.String(), "")
	opens := len(e.cr.opened)
	lists := e.cr.listCount()

	payloads := []string{
		"../status/relay.json",
		"..%2fstatus%2frelay.json",
		"%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"..%252f..",
		"....//....//x",
		sc.ClipID.String() + "/../../status/relay.json",
		sc.ClipID.String() + "%2f..%2f..",
		"01ARZ3NDEKTSV4RRFFQ69G5FAV%00",
		"..",
		".",
		"%2e%2e",
	}
	for _, p := range payloads {
		for _, suffix := range []string{"", "/video"} {
			rec := e.do("GET", "/api/v1/clips/"+p+suffix, "")
			switch rec.Code {
			case http.StatusBadRequest, http.StatusNotFound, http.StatusMovedPermanently, http.StatusPermanentRedirect, http.StatusMethodNotAllowed:
			default:
				t.Errorf("clip id %q%s: status %d", p, suffix, rec.Code)
			}
		}
	}
	e.cr.mu.Lock()
	defer e.cr.mu.Unlock()
	if len(e.cr.opened) != opens || len(e.cr.lists) != lists {
		t.Errorf("storage was touched by hostile clip ids: opened %v, lists %v", e.cr.opened[opens:], e.cr.lists[lists:])
	}
	for _, k := range e.cr.opened {
		if strings.Contains(k, "..") {
			t.Errorf("storage key %q contains ..", k)
		}
	}
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	e := newEnv(t)
	wantProblem(t, e.do("GET", "/api/v1/nope", ""), http.StatusNotFound)
	// video-link was removed with presigning (D18).
	wantProblem(t, e.do("GET", "/api/v1/clips/"+e.f.newID(utc(2026, 11, 6, 9, 0, 0)).String()+"/video-link", ""), http.StatusNotFound)
	rec := e.do("POST", "/api/v1/status", "")
	wantProblem(t, rec, http.StatusMethodNotAllowed)
	if !strings.Contains(rec.Header().Get("Allow"), "GET") {
		t.Errorf("Allow = %q", rec.Header().Get("Allow"))
	}
	if rec := e.do("HEAD", "/api/v1/status", ""); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD = %d with %d body bytes", rec.Code, rec.Body.Len())
	}
}

func TestAPIResponsesAreNotCached(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/api/v1/cameras", "")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func writeCoverage(t *testing.T, e *srvEnv, camera string, hour time.Time, ready []sidecar.Interval, dropped []sidecar.Dropped) {
	t.Helper()
	cov := sidecar.Coverage{
		Schema: sidecar.CoverageSchema, CameraID: camera, HourStart: sidecar.Time{Time: hour},
		Ready: ready, Dropped: dropped,
	}
	if ready == nil {
		cov.Ready = []sidecar.Interval{}
	}
	if dropped == nil {
		cov.Dropped = []sidecar.Dropped{}
	}
	if err := cov.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cov)
	if err := e.f.drv.PutSmall(context.Background(), sidecar.CoverageKey(camera, hour), b, "application/json"); err != nil {
		t.Fatal(err)
	}
}

func iv(a, b time.Time) sidecar.Interval {
	return sidecar.Interval{{Time: a}, {Time: b}}
}

type coverageResp struct {
	Camera  string             `json:"camera"`
	From    sidecar.Time       `json:"from"`
	To      sidecar.Time       `json:"to"`
	Ready   []sidecar.Interval `json:"ready"`
	Dropped []sidecar.Dropped  `json:"dropped"`
}

func TestCoverageMergesHoursAndDerivesCurrentHour(t *testing.T) {
	e := newEnv(t)
	// Hour 10 has no object (not ready). Hour 11 has two intervals, the first
	// touching the hour 11 start. The current hour (12) comes from relay.json.
	writeCoverage(t, e, "front", utc(2026, 11, 6, 9, 0, 0),
		[]sidecar.Interval{iv(utc(2026, 11, 6, 9, 30, 0), utc(2026, 11, 6, 10, 0, 0))}, nil)
	writeCoverage(t, e, "front", utc(2026, 11, 6, 11, 0, 0),
		[]sidecar.Interval{iv(utc(2026, 11, 6, 11, 0, 0), utc(2026, 11, 6, 11, 20, 0)), iv(utc(2026, 11, 6, 11, 30, 0), utc(2026, 11, 6, 12, 0, 0))},
		[]sidecar.Dropped{{At: sidecar.Time{Time: utc(2026, 11, 6, 11, 20, 0)}, Reason: "stream_lost", ApproxMs: 52000}})
	now := testNow.Add(20 * time.Minute)
	e.f.clk.Advance(20 * time.Minute)
	st := camStatus("front", "up")
	st.Since = sidecar.Time{Time: utc(2026, 11, 6, 11, 40, 0)}
	e.f.writeStatus(now, st)

	rec := e.do("GET", "/api/v1/coverage?camera=front&from=2026-11-06T09:00:00Z&to=2026-11-06T13:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got coverageResp
	decodeJSON(t, rec, &got)
	want := []sidecar.Interval{
		iv(utc(2026, 11, 6, 9, 30, 0), utc(2026, 11, 6, 10, 0, 0)),
		iv(utc(2026, 11, 6, 11, 0, 0), utc(2026, 11, 6, 11, 20, 0)),
		iv(utc(2026, 11, 6, 11, 30, 0), now), // 11:30-12:00 merged with the derived 12:00-12:20
	}
	if got.Camera != "front" || fmt.Sprint(got.Ready) != fmt.Sprint(want) {
		t.Errorf("ready = %v, want %v", got.Ready, want)
	}
	if len(got.Dropped) != 1 || got.Dropped[0].Reason != "stream_lost" || got.Dropped[0].ApproxMs != 52000 {
		t.Errorf("dropped = %+v", got.Dropped)
	}

	// Clamped to the query window; a dropped entry outside it is left out.
	rec = e.do("GET", "/api/v1/coverage?camera=front&from=2026-11-06T09:45:00Z&to=2026-11-06T11:10:00Z", "")
	decodeJSON(t, rec, &got)
	want = []sidecar.Interval{
		iv(utc(2026, 11, 6, 9, 45, 0), utc(2026, 11, 6, 10, 0, 0)),
		iv(utc(2026, 11, 6, 11, 0, 0), utc(2026, 11, 6, 11, 10, 0)),
	}
	if fmt.Sprint(got.Ready) != fmt.Sprint(want) || len(got.Dropped) != 0 {
		t.Errorf("clamped: ready = %v dropped = %v, want %v and none", got.Ready, got.Dropped, want)
	}
}

func TestCoverageCurrentHourNotReadyWhenRelayDownOrStale(t *testing.T) {
	for name, setup := range map[string]func(e *srvEnv){
		"events down": func(e *srvEnv) {
			st := camStatus("front", "up")
			st.Events = "down"
			e.f.writeStatus(testNow, st)
		},
		"rtsp degraded": func(e *srvEnv) { e.f.writeStatus(testNow, camStatus("front", "degraded")) },
		"stale status":  func(e *srvEnv) { e.f.writeStatus(testNow.Add(-10*time.Minute), camStatus("front", "up")) },
		"no status":     func(e *srvEnv) {},
		"other camera":  func(e *srvEnv) { e.f.writeStatus(testNow, camStatus("back", "up")) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(e)
			rec := e.do("GET", "/api/v1/coverage?camera=front&from=2026-11-06T12:00:00Z&to=2026-11-06T13:00:00Z", "")
			var got coverageResp
			decodeJSON(t, rec, &got)
			if rec.Code != 200 || len(got.Ready) != 0 {
				t.Errorf("status %d ready = %v, want none", rec.Code, got.Ready)
			}
			if got.Ready == nil || got.Dropped == nil {
				t.Errorf("ready and dropped must be [] not null: %s", rec.Body)
			}
		})
	}
}

func TestCoverageRejectsBadQueriesAndInvalidObjects(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{
		"",
		"from=2026-11-06T09:00:00Z&to=2026-11-06T10:00:00Z",              // no camera
		"camera=front&from=2026-11-06T09:00:00Z",                         // no to
		"camera=../x&from=2026-11-06T09:00:00Z&to=2026-11-06T10:00:00Z",  // bad camera
		"camera=front&from=2026-11-06T10:00:00Z&to=2026-11-06T09:00:00Z", // inverted
		"camera=front&from=2026-11-01T00:00:00Z&to=2026-11-10T00:00:00Z", // > 8 days
	} {
		wantProblem(t, e.do("GET", "/api/v1/coverage?"+q, ""), http.StatusBadRequest)
	}

	// A malformed object counts as not ready; it is not served.
	if err := e.f.drv.PutSmall(context.Background(), sidecar.CoverageKey("front", utc(2026, 11, 6, 9, 0, 0)), []byte(`{"schema":"zrec.coverage/1"}`), "application/json"); err != nil {
		t.Fatal(err)
	}
	rec := e.do("GET", "/api/v1/coverage?camera=front&from=2026-11-06T09:00:00Z&to=2026-11-06T10:00:00Z", "")
	var got coverageResp
	decodeJSON(t, rec, &got)
	if rec.Code != 200 || len(got.Ready) != 0 {
		t.Errorf("invalid coverage object: status %d ready %v", rec.Code, got.Ready)
	}
}
