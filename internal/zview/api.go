package zview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// problem is an RFC 9457 problem document.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// writeProblem answers with application/problem+json. detail must not
// carry internal error text.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Length")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail})
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("zview: encode response", "err", err)
		writeProblem(w, r, http.StatusInternalServerError, "")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

// getOnly allows GET (and HEAD, which the handlers answer without a body).
func getOnly(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeProblem(w, r, http.StatusMethodNotAllowed, "")
			return
		}
		next(w, r)
	})
}

// storageFailure answers a non-sentinel index or storage error. The error
// text stays in the log: it may name keys or endpoints.
func storageFailure(w http.ResponseWriter, r *http.Request, op string, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	slog.Error("zview: storage error", "op", op, "err", err)
	writeProblem(w, r, http.StatusBadGateway, "storage is unavailable")
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, "")
}

// status passes through status/relay.json (including its storage object)
// and adds the index stats (ui-spec §3.4).
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if st := s.idx.relayStatus(r.Context()); st != nil {
		b, err := json.Marshal(st)
		if err == nil {
			err = json.Unmarshal(b, &out)
		}
		if err != nil {
			storageFailure(w, r, "status", err)
			return
		}
	}
	stats := s.idx.Stats()
	out["indexed_days"] = stats.IndexedDays
	out["invalid_sidecars"] = stats.InvalidSidecars
	if stats.IndexFreshAt.IsZero() {
		out["index_fresh_at"] = nil
	} else {
		out["index_fresh_at"] = sidecar.Time{Time: stats.IndexFreshAt.UTC()}
	}
	writeJSON(w, r, out)
}

func (s *server) cameras(w http.ResponseWriter, r *http.Request) {
	cams, err := s.idx.Cameras(r.Context())
	if err != nil {
		storageFailure(w, r, "cameras", err)
		return
	}
	if cams == nil {
		cams = []CameraSummary{}
	}
	writeJSON(w, r, cams)
}

// parseWindow reads the required RFC 3339 from and to parameters.
func parseWindow(r *http.Request) (from, to time.Time, detail string) {
	q := r.URL.Query()
	var err error
	if from, err = time.Parse(time.RFC3339, q.Get("from")); err != nil {
		return from, to, "from must be an RFC 3339 time"
	}
	if to, err = time.Parse(time.RFC3339, q.Get("to")); err != nil {
		return from, to, "to must be an RFC 3339 time"
	}
	switch {
	case !to.After(from):
		return from, to, ErrBadRange.Error()
	case to.Sub(from) > MaxRange:
		return from, to, ErrRangeTooLarge.Error()
	}
	return from.UTC(), to.UTC(), ""
}

var triggerKinds = []events.Kind{events.KindMotion, events.KindPerson, events.KindVehicle, events.KindPet, events.KindOther}

func (s *server) clips(w http.ResponseWriter, r *http.Request) {
	from, to, detail := parseWindow(r)
	if detail != "" {
		writeProblem(w, r, http.StatusBadRequest, detail)
		return
	}
	q := r.URL.Query()
	cameras := q["camera"]
	for _, c := range cameras {
		if !cameraIDRE.MatchString(c) {
			writeProblem(w, r, http.StatusBadRequest, "camera is not a valid camera id")
			return
		}
	}
	var triggers []events.Kind
	for _, t := range q["trigger"] {
		k := events.Kind(t)
		if !slices.Contains(triggerKinds, k) {
			writeProblem(w, r, http.StatusBadRequest, "unknown trigger kind")
			return
		}
		triggers = append(triggers, k)
	}
	sums, err := s.idx.Clips(r.Context(), from, to, cameras, triggers)
	switch {
	case errors.Is(err, ErrBadRange), errors.Is(err, ErrRangeTooLarge):
		writeProblem(w, r, http.StatusBadRequest, err.Error())
	case err != nil:
		storageFailure(w, r, "clips", err)
	default:
		writeJSON(w, r, sums)
	}
}

// clipID parses the {clip_id} path value. Only a canonical ULID gets past
// here, so no client text ever becomes part of a storage key.
func clipID(w http.ResponseWriter, r *http.Request) (ulid.ULID, bool) {
	id, err := ulid.ParseStrict(r.PathValue("clip_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "clip_id is not a ULID")
		return ulid.ULID{}, false
	}
	return id, true
}

// lookup resolves a clip, answering the HTTP error itself on failure.
func (s *server) lookup(w http.ResponseWriter, r *http.Request) (*sidecar.Sidecar, string, bool) {
	id, ok := clipID(w, r)
	if !ok {
		return nil, "", false
	}
	sc, key, err := s.idx.Clip(r.Context(), id)
	switch {
	case errors.Is(err, ErrClipNotFound):
		writeProblem(w, r, http.StatusNotFound, "clip not found")
		return nil, "", false
	case err != nil:
		storageFailure(w, r, "clip", err)
		return nil, "", false
	}
	return sc, key, true
}

// clip returns the full sidecar verbatim plus video_href.
func (s *server) clip(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.lookup(w, r)
	if !ok {
		return
	}
	b, err := json.Marshal(sc)
	var fields map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(b, &fields)
	}
	if err != nil {
		storageFailure(w, r, "clip", err)
		return
	}
	href, _ := json.Marshal(newSummary(sc).VideoHref)
	fields["video_href"] = href
	writeJSON(w, r, fields)
}

type rangeKind int

const (
	rangeNone          rangeKind = iota // no usable Range header: serve everything
	rangePartial                        // serve [start, end]
	rangeUnsatisfiable                  // 416
)

// parseRange interprets a Range header against an object of size bytes.
// Only a single bytes range is served; anything else is ignored, as RFC 9110
// §14.2 allows.
func parseRange(h string, size int64) (start, end int64, kind rangeKind) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, rangeNone
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, rangeNone
	}
	if a == "" { // suffix: the last b bytes
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, rangeNone
		}
		if n == 0 || size == 0 {
			return 0, 0, rangeUnsatisfiable
		}
		return max(size-n, 0), size - 1, rangePartial
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, rangeNone
	}
	end = size - 1
	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < 0 {
			return 0, 0, rangeNone
		}
		if e < start {
			return 0, 0, rangeUnsatisfiable
		}
		end = min(e, size-1)
	}
	if start >= size {
		return 0, 0, rangeUnsatisfiable
	}
	return start, end, rangePartial
}

// video streams the ciphertext object with Range and ETag support. The key
// comes from the index, never from the request.
func (s *server) video(w http.ResponseWriter, r *http.Request) {
	_, key, ok := s.lookup(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	info, err := s.store.Stat(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "video not found")
		return
	}
	if err != nil {
		storageFailure(w, r, "video stat", err)
		return
	}
	etag := quoteETag(info.ETag)
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	if etag != "" {
		h.Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	start, end, kind := parseRange(r.Header.Get("Range"), info.Size)
	if kind == rangeUnsatisfiable {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		h.Del("Cache-Control")
		writeProblem(w, r, http.StatusRequestedRangeNotSatisfiable, "")
		return
	}
	off, n, status := int64(0), int64(-1), http.StatusOK
	length := info.Size
	if kind == rangePartial {
		off, n, status = start, end-start+1, http.StatusPartialContent
		length = n
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	rng, err := s.store.OpenRange(ctx, key, off, n)
	if errors.Is(err, storage.ErrNotFound) {
		h.Del("Content-Range")
		h.Del("ETag")
		writeProblem(w, r, http.StatusNotFound, "video not found")
		return
	}
	if err != nil {
		h.Del("Content-Range")
		storageFailure(w, r, "video open", err)
		return
	}
	defer rng.Close()
	w.WriteHeader(status)
	// A failure now cannot change the status; the client sees a short body.
	if _, err := io.Copy(w, io.LimitReader(rng, length)); err != nil && ctx.Err() == nil {
		slog.Warn("zview: video copy failed", "err", err)
	}
}

// quoteETag returns etag as an HTTP entity tag. S3 reports it quoted, the
// local driver bare.
func quoteETag(etag string) string {
	if etag == "" || strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, "W/") {
		return etag
	}
	return `"` + etag + `"`
}

// etagMatches reports whether an If-None-Match value lists etag (weak
// comparison, RFC 9110 §13.1.2).
func etagMatches(header, etag string) bool {
	for _, c := range strings.Split(header, ",") {
		c = strings.TrimSpace(c)
		if c == "*" || strings.TrimPrefix(c, "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}
	return false
}

// coverageResponse is the body of GET /api/v1/coverage.
type coverageResponse struct {
	Camera  string             `json:"camera"`
	From    sidecar.Time       `json:"from"`
	To      sidecar.Time       `json:"to"`
	Ready   []sidecar.Interval `json:"ready"`
	Dropped []sidecar.Dropped  `json:"dropped"`
}

// coverage merges the ready intervals and dropped entries of the hourly
// coverage objects overlapping the window (ui-spec §3.4). An hour with no
// object is not ready, except the current hour, which is derived from
// status/relay.json.
func (s *server) coverage(w http.ResponseWriter, r *http.Request) {
	camera := r.URL.Query().Get("camera")
	if !cameraIDRE.MatchString(camera) {
		writeProblem(w, r, http.StatusBadRequest, "camera is required and must be a valid camera id")
		return
	}
	from, to, detail := parseWindow(r)
	if detail != "" {
		writeProblem(w, r, http.StatusBadRequest, detail)
		return
	}
	ctx := r.Context()
	var hours []time.Time
	for h := from.Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		hours = append(hours, h)
	}
	covs := make([]*sidecar.Coverage, len(hours))
	errs := make([]error, len(hours))
	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchWorkers)
	for i, h := range hours {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			covs[i], errs[i] = s.idx.coverageHour(ctx, camera, h)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		storageFailure(w, r, "coverage", err)
		return
	}

	out := coverageResponse{Camera: camera, From: sidecar.Time{Time: from}, To: sidecar.Time{Time: to},
		Ready: []sidecar.Interval{}, Dropped: []sidecar.Dropped{}}
	var ready [][2]time.Time
	for _, c := range covs {
		if c == nil {
			continue
		}
		for _, iv := range c.Ready {
			ready = append(ready, [2]time.Time{iv[0].Time, iv[1].Time})
		}
		for _, d := range c.Dropped {
			if !d.At.Before(from) && d.At.Before(to) {
				out.Dropped = append(out.Dropped, d)
			}
		}
	}
	if iv, ok := s.idx.currentHourReady(ctx, camera); ok {
		ready = append(ready, iv)
	}
	for _, iv := range mergeIntervals(ready, from, to) {
		out.Ready = append(out.Ready, sidecar.Interval{{Time: iv[0]}, {Time: iv[1]}})
	}
	slices.SortStableFunc(out.Dropped, func(a, b sidecar.Dropped) int { return a.At.Compare(b.At.Time) })
	writeJSON(w, r, out)
}

// mergeIntervals clamps intervals to [from, to], drops empty ones and merges
// overlapping or touching ones, in ascending order.
func mergeIntervals(in [][2]time.Time, from, to time.Time) [][2]time.Time {
	var cl [][2]time.Time
	for _, iv := range in {
		a, b := iv[0], iv[1]
		if a.Before(from) {
			a = from
		}
		if b.After(to) {
			b = to
		}
		if a.Before(b) {
			cl = append(cl, [2]time.Time{a, b})
		}
	}
	slices.SortFunc(cl, func(x, y [2]time.Time) int { return x[0].Compare(y[0]) })
	var out [][2]time.Time
	for _, iv := range cl {
		if n := len(out); n > 0 && !iv[0].After(out[n-1][1]) {
			if iv[1].After(out[n-1][1]) {
				out[n-1][1] = iv[1]
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// coverageHour reads and validates the coverage object of one UTC hour. A
// missing or invalid object yields nil (not ready); a storage failure is an
// error, so an outage is not reported as "not monitored".
func (x *Index) coverageHour(ctx context.Context, camera string, hour time.Time) (*sidecar.Coverage, error) {
	key := sidecar.CoverageKey(camera, hour)
	b, err := x.readObject(ctx, key, maxStatusBytes)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return nil, nil
	case errors.Is(err, errTooLarge):
		slog.Warn("zview: oversized coverage object", "key", key)
		return nil, nil
	case err != nil:
		return nil, err
	}
	var c sidecar.Coverage
	if err := json.Unmarshal(b, &c); err != nil || c.Validate() != nil || c.CameraID != camera || !c.HourStart.Equal(hour) {
		slog.Warn("zview: invalid coverage object", "key", key)
		return nil, nil
	}
	return &c, nil
}

// currentHourReady derives the ready interval of the current UTC hour from
// status/relay.json: the camera has been up since max(hour start, since) as
// of the status' updated_at. A missing, stale or non-up status gives none.
func (x *Index) currentHourReady(ctx context.Context, camera string) ([2]time.Time, bool) {
	now := x.clk.Now().UTC()
	st := x.relayStatus(ctx)
	if !statusFresh(st, now) {
		return [2]time.Time{}, false
	}
	for _, c := range st.Cameras {
		if c.ID != camera || c.RTSP != "up" || c.Events != "up" {
			continue
		}
		start := c.Since.Time
		if h := now.Truncate(time.Hour); start.Before(h) {
			start = h
		}
		if end := st.UpdatedAt.Time; end.After(start) {
			return [2]time.Time{start, end}, true
		}
	}
	return [2]time.Time{}, false
}
