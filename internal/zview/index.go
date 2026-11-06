package zview

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

const (
	// MaxRange is the longest window Clips accepts (ui-spec §3.4).
	MaxRange = 8 * 24 * time.Hour

	// statusStaleAfter is the age of status/relay.json past which the relay
	// is considered down (Contract §7.5.1).
	statusStaleAfter = 3 * time.Minute

	maxSidecarBytes = storage.MaxSmallSize
	maxStatusBytes  = storage.MaxSmallSize
	fetchWorkers    = 8
	dayLen          = 24 * time.Hour
)

var (
	// ErrBadRange reports a clip window whose end is not after its start.
	ErrBadRange = errors.New("zview: range end must be after its start")
	// ErrRangeTooLarge reports a clip window longer than MaxRange.
	ErrRangeTooLarge = errors.New("zview: range exceeds 8 days")
	// ErrClipNotFound reports a clip id that no valid indexed sidecar has.
	ErrClipNotFound = errors.New("zview: clip not found")

	errTooLarge = errors.New("zview: object too large")
)

// VideoSummary is sidecar.Video without seek_index.
type VideoSummary struct {
	Object             string `json:"object"`
	FormatVersion      int    `json:"format_version"`
	KeyID              string `json:"key_id"`
	ChunkSize          int64  `json:"chunk_size"`
	SizeBytes          int64  `json:"size_bytes"`
	PlaintextSizeBytes *int64 `json:"plaintext_size_bytes"`
	Container          string `json:"container"`
	Codec              string `json:"codec"`
	Width              int    `json:"width"`
	Height             int    `json:"height"`
	InitSegmentBytes   int64  `json:"init_segment_bytes"`
}

// Summary is a clip as listed by GET /api/v1/clips: the sidecar minus
// video.seek_index, plus VideoHref (ui-spec §3.4).
type Summary struct {
	Schema       string            `json:"schema"`
	Status       string            `json:"status"`
	ClipID       ulid.ULID         `json:"clip_id"`
	SeriesID     ulid.ULID         `json:"series_id"`
	SegmentIndex int               `json:"segment_index"`
	Camera       sidecar.Camera    `json:"camera"`
	StartTime    sidecar.Time      `json:"start_time"`
	EndTime      *sidecar.Time     `json:"end_time"`
	DurationMs   *int64            `json:"duration_ms"`
	PreRollMs    int64             `json:"pre_roll_ms"`
	EndReason    string            `json:"end_reason"`
	Triggers     []sidecar.Trigger `json:"triggers"`
	Video        VideoSummary      `json:"video"`
	Recorder     sidecar.Recorder  `json:"recorder"`
	VideoHref    string            `json:"video_href"`
}

func newSummary(sc *sidecar.Sidecar) Summary {
	v := sc.Video
	return Summary{
		Schema:       sc.Schema,
		Status:       sc.Status,
		ClipID:       sc.ClipID,
		SeriesID:     sc.SeriesID,
		SegmentIndex: sc.SegmentIndex,
		Camera:       sc.Camera,
		StartTime:    sc.StartTime,
		EndTime:      sc.EndTime,
		DurationMs:   sc.DurationMs,
		PreRollMs:    sc.PreRollMs,
		EndReason:    sc.EndReason,
		Triggers:     sc.Triggers,
		Video: VideoSummary{
			Object: v.Object, FormatVersion: v.FormatVersion, KeyID: v.KeyID,
			ChunkSize: v.ChunkSize, SizeBytes: v.SizeBytes, PlaintextSizeBytes: v.PlaintextSizeBytes,
			Container: v.Container, Codec: v.Codec, Width: v.Width, Height: v.Height,
			InitSegmentBytes: v.InitSegmentBytes,
		},
		Recorder:  sc.Recorder,
		VideoHref: "/api/v1/clips/" + sc.ClipID.String() + "/video",
	}
}

// CameraSummary is one entry of GET /api/v1/cameras.
type CameraSummary struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Brand      string        `json:"brand"`
	Tags       []string      `json:"tags"`
	LastClipAt *sidecar.Time `json:"last_clip_at"`
	Online     bool          `json:"online"`
}

// IndexStats are the index figures GET /api/v1/status adds (ui-spec §3.4).
type IndexStats struct {
	IndexedDays     int       `json:"indexed_days"`
	InvalidSidecars int       `json:"invalid_sidecars"`
	IndexFreshAt    time.Time `json:"index_fresh_at"`
}

// Index is the lazy per-UTC-day clip index over a storage.ReadProvider
// (ui-spec §3.5). Hot days (yesterday, today and tomorrow, UTC) are
// re-listed at most every hot_refresh_seconds, triggered by requests. Other
// days are loaded on first use and kept in an LRU of cold_cache_days.
// Sidecars are read once per object version.
type Index struct {
	r   storage.ReadProvider
	clk clock.Clock

	fallback  []string
	hot       time.Duration
	coldCap   int
	assumed   time.Duration
	retention time.Duration

	mu   sync.Mutex // guards days and tick
	days map[time.Time]*dayState
	tick uint64

	statusMu sync.Mutex // guards status
	status   statusCache
}

// NewIndex returns an Index reading from r. Zero HotRefreshSeconds,
// ColdCacheDays and AssumedMaxClipSeconds select the defaults; a zero
// RetentionDays turns retention invalidation off.
func NewIndex(r storage.ReadProvider, cfg IndexConfig, clk clock.Clock) *Index {
	orDefault := func(v, d int) int {
		if v <= 0 {
			return d
		}
		return v
	}
	x := &Index{
		r:         r,
		clk:       clk,
		hot:       time.Duration(orDefault(cfg.HotRefreshSeconds, DefaultHotRefreshSeconds)) * time.Second,
		coldCap:   orDefault(cfg.ColdCacheDays, DefaultColdCacheDays),
		assumed:   time.Duration(orDefault(cfg.AssumedMaxClipSeconds, DefaultAssumedMaxClipSeconds)) * time.Second,
		retention: time.Duration(max(cfg.RetentionDays, 0)) * dayLen,
		days:      make(map[time.Time]*dayState),
	}
	x.fallback = validCameraIDs(cfg.CamerasFallback)
	return x
}

type dayState struct {
	used uint64 // LRU tick, guarded by Index.mu
	mu   sync.Mutex
	snap atomic.Pointer[daySnap]
}

// daySnap is the immutable result of indexing one UTC day.
type daySnap struct {
	loadedAt time.Time
	cams     []string                  // camera set listed, sorted
	parsed   map[string]*parsedSidecar // by sidecar key, for incremental refresh
	clips    []*clipEntry              // valid clips, sorted by start
	byID     map[ulid.ULID]*clipEntry
	bySeries map[seriesKey]*clipEntry
	invalid  int
}

type seriesKey struct {
	camera  string
	series  ulid.ULID
	segment int
}

type parsedSidecar struct {
	info storage.ObjectInfo
	sum  *Summary // nil: the sidecar is invalid
}

type clipEntry struct {
	sum        Summary
	sidecarKey string
	videoKey   string
	start, end time.Time
}

var emptySnap = &daySnap{byID: map[ulid.ULID]*clipEntry{}, bySeries: map[seriesKey]*clipEntry{}}

func dayOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// isHot reports whether day d is refreshed on a timer-like cadence:
// yesterday, today and tomorrow in UTC.
func isHot(d, now time.Time) bool {
	return !d.Before(dayOf(now).AddDate(0, 0, -1))
}

// Clips lists the clips overlapping [from, to) (start < to && end > from),
// filtered by camera and trigger kind when those are non-empty, sorted by
// start_time. An incomplete clip's end is start + assumed_max_clip_seconds.
func (x *Index) Clips(ctx context.Context, from, to time.Time, cameras []string, triggers []events.Kind) ([]Summary, error) {
	if !to.After(from) {
		return nil, ErrBadRange
	}
	if to.Sub(from) > MaxRange {
		return nil, ErrRangeTooLarge
	}
	from, to = from.UTC(), to.UTC()
	var camFilter map[string]bool
	if len(cameras) > 0 {
		camFilter = make(map[string]bool, len(cameras))
		for _, c := range cameras {
			camFilter[c] = true
		}
	}
	var trigFilter map[string]bool
	if len(triggers) > 0 {
		trigFilter = make(map[string]bool, len(triggers))
		for _, k := range triggers {
			trigFilter[string(k)] = true
		}
	}

	out := []Summary{}
	// A clip is filed under the day it starts, and may run past midnight into
	// the window, so the previous day is searched too.
	last := dayOf(to.Add(-time.Nanosecond))
	for d := dayOf(from).AddDate(0, 0, -1); !d.After(last); d = d.AddDate(0, 0, 1) {
		snap, err := x.day(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, e := range snap.clips {
			if !e.start.Before(to) || !e.end.After(from) {
				continue
			}
			if camFilter != nil && !camFilter[e.sum.Camera.ID] {
				continue
			}
			if trigFilter != nil && !hasTrigger(e.sum.Triggers, trigFilter) {
				continue
			}
			out = append(out, e.sum)
		}
	}
	slices.SortStableFunc(out, func(a, b Summary) int { return a.StartTime.Compare(b.StartTime.Time) })
	return out, nil
}

func hasTrigger(trs []sidecar.Trigger, want map[string]bool) bool {
	for _, t := range trs {
		if want[t.Type] {
			return true
		}
	}
	return false
}

// Clip returns the full sidecar of clip id and the storage key of its video
// (dirname(sidecar key) + "/" + video.object). The day comes from the ULID
// timestamp (Contract §4.1); no client-supplied path reaches storage.
func (x *Index) Clip(ctx context.Context, id ulid.ULID) (*sidecar.Sidecar, string, error) {
	snap, err := x.day(ctx, dayOf(ulid.Time(id.Time())))
	if err != nil {
		return nil, "", err
	}
	e := snap.byID[id]
	if e == nil {
		return nil, "", ErrClipNotFound
	}
	b, err := x.readObject(ctx, e.sidecarKey, maxSidecarBytes)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, errTooLarge) {
		return nil, "", ErrClipNotFound
	}
	if err != nil {
		return nil, "", err
	}
	sc, err := decodeValid(b, e.sidecarKey)
	if err != nil || sc.ClipID != id {
		return nil, "", ErrClipNotFound
	}
	return sc, path.Dir(e.sidecarKey) + "/" + sc.Video.Object, nil
}

// NextInSeries returns the clip that continues s's series (same camera and
// series_id, segment_index + 1), looking in s's day and the next, or nil when
// there is none.
func (x *Index) NextInSeries(ctx context.Context, s *sidecar.Sidecar) (*Summary, error) {
	key := seriesKey{camera: s.Camera.ID, series: s.SeriesID, segment: s.SegmentIndex + 1}
	d := dayOf(s.StartTime.Time)
	for _, dd := range []time.Time{d, d.AddDate(0, 0, 1)} {
		snap, err := x.day(ctx, dd)
		if err != nil {
			return nil, err
		}
		if e := snap.bySeries[key]; e != nil {
			sum := e.sum
			return &sum, nil
		}
	}
	return nil, nil
}

// Cameras merges the cameras of status/relay.json, the fallback list in use
// and the cameras seen in indexed sidecars. Cameras are sorted by id.
func (x *Index) Cameras(ctx context.Context) ([]CameraSummary, error) {
	now := x.clk.Now()
	st := x.relayStatus(ctx)
	fresh := statusFresh(st, now)
	byID := map[string]*CameraSummary{}
	get := func(id string) *CameraSummary {
		c := byID[id]
		if c == nil {
			c = &CameraSummary{ID: id, Name: id, Tags: []string{}}
			byID[id] = c
		}
		return c
	}
	if st != nil {
		for _, cs := range st.Cameras {
			if !cameraIDRE.MatchString(cs.ID) {
				continue
			}
			c := get(cs.ID)
			if cs.Name != "" {
				c.Name = cs.Name
			}
			if cs.LastClipAt != nil {
				t := *cs.LastClipAt
				c.LastClipAt = &t
			}
			c.Online = fresh && cs.RTSP == "up"
		}
	}
	for _, id := range x.cameraSet(ctx) {
		get(id)
	}

	named := map[string]bool{}
	for _, s := range x.snapshots() {
		for _, e := range s.clips {
			c := get(e.sum.Camera.ID)
			latest := c.LastClipAt == nil || e.start.After(c.LastClipAt.Time)
			if latest {
				t := e.sum.StartTime
				c.LastClipAt = &t
			}
			if latest || !named[c.ID] {
				named[c.ID] = true
				c.Brand = e.sum.Camera.Brand
				if e.sum.Camera.Tags != nil {
					c.Tags = e.sum.Camera.Tags
				}
				if st == nil || !statusNames(st, c.ID) {
					c.Name = e.sum.Camera.Name
				}
			}
		}
	}

	out := make([]CameraSummary, 0, len(byID))
	for _, c := range byID {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b CameraSummary) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func statusNames(st *sidecar.Status, id string) bool {
	for _, c := range st.Cameras {
		if c.ID == id && c.Name != "" {
			return true
		}
	}
	return false
}

// Stats reports what is currently indexed. It never touches storage.
func (x *Index) Stats() IndexStats {
	var s IndexStats
	for _, snap := range x.snapshots() {
		s.IndexedDays++
		s.InvalidSidecars += snap.invalid
		if snap.loadedAt.After(s.IndexFreshAt) {
			s.IndexFreshAt = snap.loadedAt
		}
	}
	return s
}

func (x *Index) snapshots() []*daySnap {
	x.mu.Lock()
	states := make([]*dayState, 0, len(x.days))
	for _, st := range x.days {
		states = append(states, st)
	}
	x.mu.Unlock()
	out := make([]*daySnap, 0, len(states))
	for _, st := range states {
		if s := st.snap.Load(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// day returns the index of UTC day d, loading or refreshing it as needed.
func (x *Index) day(ctx context.Context, d time.Time) (*daySnap, error) {
	if d.After(dayOf(x.clk.Now()).AddDate(0, 0, 1)) {
		return emptySnap, nil // after tomorrow no clip can exist yet
	}
	x.mu.Lock()
	st := x.days[d]
	if st == nil {
		st = &dayState{}
		x.days[d] = st
	}
	x.tick++
	st.used = x.tick
	x.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	cams := x.cameraSet(ctx)
	now := x.clk.Now()
	prev := st.snap.Load()
	if prev != nil && !x.needsRefresh(prev, d, now, cams) {
		return prev, nil
	}
	snap, err := x.load(ctx, d, cams, prev, now)
	if err != nil {
		return nil, err
	}
	st.snap.Store(snap)
	x.mu.Lock()
	x.evictLocked(now)
	x.mu.Unlock()
	return snap, nil
}

func (x *Index) needsRefresh(snap *daySnap, d, now time.Time, cams []string) bool {
	for _, c := range cams {
		if _, ok := slices.BinarySearch(snap.cams, c); !ok {
			return true // the camera set grew since this day was listed
		}
	}
	if isHot(d, now) {
		return now.Sub(snap.loadedAt) >= x.hot
	}
	if snap.loadedAt.Before(d.AddDate(0, 0, 2)) {
		return true // loaded while hot, so it may predate late uploads: reload once
	}
	if x.retention > 0 && now.Sub(d.AddDate(0, 0, 1)) >= x.retention {
		// Past retention_days the day may have been deleted by ILM
		// (design §6.7): a cheap re-list, rate limited like a hot day.
		return now.Sub(snap.loadedAt) >= x.hot
	}
	return false
}

// evictLocked drops least recently used cold days beyond the LRU capacity.
func (x *Index) evictLocked(now time.Time) {
	for {
		var victim time.Time
		var oldest uint64
		n := 0
		for d, st := range x.days {
			if isHot(d, now) {
				continue
			}
			n++
			if n == 1 || st.used < oldest {
				victim, oldest = d, st.used
			}
		}
		if n <= x.coldCap {
			return
		}
		delete(x.days, victim)
	}
}

// load lists day d for every camera and builds its snapshot. Sidecars whose
// object version is unchanged since prev are not read again.
func (x *Index) load(ctx context.Context, d time.Time, cams []string, prev *daySnap, now time.Time) (*daySnap, error) {
	listing := map[string]storage.ObjectInfo{}
	for _, cam := range cams {
		dir := sidecar.DayPrefix(cam, d)
		for info, err := range x.r.List(ctx, dir) {
			if err != nil {
				return nil, fmt.Errorf("zview: list %s: %w", dir, err)
			}
			if path.Dir(info.Key) == dir {
				listing[info.Key] = info
			}
		}
	}

	parsed := make(map[string]*parsedSidecar)
	var todo []storage.ObjectInfo
	for key, info := range listing {
		if !isSidecarName(path.Base(key)) {
			continue
		}
		if p := prev.parsedFor(key); p != nil && sameVersion(p.info, info) {
			parsed[key] = p
			continue
		}
		todo = append(todo, info)
	}
	fetched, err := x.fetchAll(ctx, todo)
	if err != nil {
		return nil, err
	}
	for _, p := range fetched {
		if p != nil {
			parsed[p.info.Key] = p
		}
	}

	snap := &daySnap{
		loadedAt: now,
		cams:     slices.Clone(cams),
		parsed:   parsed,
		byID:     make(map[ulid.ULID]*clipEntry),
		bySeries: make(map[seriesKey]*clipEntry),
	}
	for key, p := range parsed {
		if p.sum == nil {
			snap.invalid++
			continue
		}
		videoKey := path.Dir(key) + "/" + p.sum.Video.Object
		if _, ok := listing[videoKey]; !ok {
			slog.Debug("zview: sidecar video missing from listing", "key", key)
			snap.invalid++
			continue
		}
		start := p.sum.StartTime.Time
		end := start.Add(x.assumed)
		if p.sum.EndTime != nil {
			end = p.sum.EndTime.Time
		}
		e := &clipEntry{sum: *p.sum, sidecarKey: key, videoKey: videoKey, start: start, end: end}
		if _, dup := snap.byID[e.sum.ClipID]; dup {
			snap.invalid++
			continue
		}
		snap.clips = append(snap.clips, e)
		snap.byID[e.sum.ClipID] = e
		snap.bySeries[seriesKey{e.sum.Camera.ID, e.sum.SeriesID, e.sum.SegmentIndex}] = e
	}
	slices.SortFunc(snap.clips, func(a, b *clipEntry) int {
		if c := a.start.Compare(b.start); c != 0 {
			return c
		}
		return a.sum.ClipID.Compare(b.sum.ClipID)
	})
	return snap, nil
}

func (s *daySnap) parsedFor(key string) *parsedSidecar {
	if s == nil {
		return nil
	}
	return s.parsed[key]
}

func sameVersion(a, b storage.ObjectInfo) bool {
	return a.ETag == b.ETag && a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

func isSidecarName(name string) bool {
	return sidecar.NameRE.MatchString(name) && path.Ext(name) == ".json"
}

// fetchAll reads and parses sidecars with bounded concurrency. A sidecar
// that vanished (retention) is skipped; a storage failure aborts the load.
func (x *Index) fetchAll(ctx context.Context, infos []storage.ObjectInfo) ([]*parsedSidecar, error) {
	out := make([]*parsedSidecar, len(infos))
	if len(infos) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		sem      = make(chan struct{}, fetchWorkers)
	)
	for i, info := range infos {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			p, err := x.parseSidecar(ctx, info)
			if err != nil {
				errOnce.Do(func() { firstErr = err; cancel() })
				return
			}
			out[i] = p
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// parseSidecar reads, decodes and validates one sidecar (ui-spec §3.5). An
// invalid sidecar is a result (sum == nil), logged once; only a storage
// failure is an error. A sidecar deleted since the listing yields nil.
func (x *Index) parseSidecar(ctx context.Context, info storage.ObjectInfo) (*parsedSidecar, error) {
	bad := func(err error) (*parsedSidecar, error) {
		slog.Warn("zview: invalid sidecar", "key", info.Key, "reason", err.Error())
		return &parsedSidecar{info: info}, nil
	}
	b, err := x.readObject(ctx, info.Key, maxSidecarBytes)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return nil, nil
	case errors.Is(err, errTooLarge):
		return bad(err)
	case err != nil:
		return nil, fmt.Errorf("zview: read %s: %w", info.Key, err)
	}
	sc, err := decodeValid(b, info.Key)
	if err != nil {
		return bad(err)
	}
	sum := newSummary(sc)
	return &parsedSidecar{info: info, sum: &sum}, nil
}

// decodeValid decodes and validates a sidecar and checks that its clip_id is
// the ULID in its filename.
func decodeValid(b []byte, key string) (*sidecar.Sidecar, error) {
	sc, err := sidecar.Decode(b)
	if err != nil {
		return nil, err
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	name := path.Base(key)
	// NameRE: 9 digits, "_", 26 ULID characters, extension.
	if len(name) < 36 || name[10:36] != sc.ClipID.String() {
		return nil, errors.New("clip_id does not match the filename")
	}
	return sc, nil
}

// readObject reads a whole object of at most max bytes.
func (x *Index) readObject(ctx context.Context, key string, max int64) ([]byte, error) {
	rc, err := x.r.OpenRange(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if rc.Total > max {
		return nil, errTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errTooLarge
	}
	return b, nil
}

// statusCache holds the last read of status/relay.json.
type statusCache struct {
	fetched time.Time
	have    bool
	status  *sidecar.Status // nil: missing, unreadable or not zrec.status/1
}

// relayStatus returns status/relay.json, re-read at most every hot-refresh
// interval. It returns nil when the object is missing or unusable.
func (x *Index) relayStatus(ctx context.Context) *sidecar.Status {
	x.statusMu.Lock()
	defer x.statusMu.Unlock()
	now := x.clk.Now()
	if x.status.have && now.Sub(x.status.fetched) < x.hot {
		return x.status.status
	}
	var st *sidecar.Status
	b, err := x.readObject(ctx, sidecar.StatusKey, maxStatusBytes)
	switch {
	case err == nil:
		var s sidecar.Status
		if jerr := json.Unmarshal(b, &s); jerr != nil || s.Schema != sidecar.StatusSchema {
			slog.Warn("zview: unusable status/relay.json")
		} else {
			st = &s
		}
	case errors.Is(err, storage.ErrNotFound):
	default:
		if ctx.Err() != nil {
			return x.status.status // do not cache a cancelled read
		}
		slog.Warn("zview: cannot read status/relay.json", "err", err)
	}
	x.status = statusCache{fetched: now, have: true, status: st}
	return st
}

func statusFresh(st *sidecar.Status, now time.Time) bool {
	return st != nil && now.Sub(st.UpdatedAt.Time) <= statusStaleAfter
}

// cameraSet returns the sorted camera ids to list. status/relay.json is
// authoritative (Contract §7.5.1); cameras_fallback is used only when it is
// missing or stale. A stale status with no fallback configured still names
// its cameras, as the only camera list there is.
func (x *Index) cameraSet(ctx context.Context) []string {
	st := x.relayStatus(ctx)
	if st != nil && (statusFresh(st, x.clk.Now()) || len(x.fallback) == 0) {
		ids := make([]string, 0, len(st.Cameras))
		for _, c := range st.Cameras {
			ids = append(ids, c.ID)
		}
		return validCameraIDs(ids)
	}
	return x.fallback
}

// validCameraIDs returns the distinct ids matching the camera id pattern,
// sorted. Anything else never reaches a storage prefix.
func validCameraIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		if cameraIDRE.MatchString(id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
