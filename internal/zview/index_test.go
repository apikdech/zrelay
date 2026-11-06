package zview

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
)

var frontOnly = IndexConfig{CamerasFallback: []string{"front"}}

func mustClips(t *testing.T, idx *Index, from, to time.Time, cams []string, trig []events.Kind) []Summary {
	t.Helper()
	got, err := idx.Clips(context.Background(), from, to, cams, trig)
	if err != nil {
		t.Fatalf("Clips: %v", err)
	}
	return got
}

func TestClipsOverlapRule(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	before := f.addClip(clipOpt{Start: day.Add(9*time.Hour + 59*time.Minute + 30*time.Second), Dur: 30 * time.Second}) // ends exactly at 10:00
	inside := f.addClip(clipOpt{Start: day.Add(10*time.Hour + 5*time.Minute)})
	straddleStart := f.addClip(clipOpt{Start: day.Add(9*time.Hour + 59*time.Minute + 45*time.Second), Dur: 30 * time.Second})
	atTo := f.addClip(clipOpt{Start: day.Add(11 * time.Hour)}) // starts exactly at to
	idx := f.index(f.drv, frontOnly)

	got := mustClips(t, idx, day.Add(10*time.Hour), day.Add(11*time.Hour), nil, nil)
	if !sameIDs(got, straddleStart, inside) {
		t.Fatalf("overlap = %v, want [%s %s] (excluding end==from %s and start==to %s)",
			ids(got), straddleStart.ClipID, inside.ClipID, before.ClipID, atTo.ClipID)
	}
}

func TestIncompleteClipUsesAssumedMaxClipSeconds(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	inc := f.addClip(clipOpt{Start: day.Add(10 * time.Hour), Incomplete: true})
	idx := f.index(f.drv, IndexConfig{CamerasFallback: []string{"front"}, AssumedMaxClipSeconds: 300})

	// end = start + 300 s = 10:05:00.
	if got := mustClips(t, idx, day.Add(10*time.Hour+4*time.Minute), day.Add(11*time.Hour), nil, nil); !sameIDs(got, inc) {
		t.Errorf("window inside assumed end: got %v", ids(got))
	}
	if got := mustClips(t, idx, day.Add(10*time.Hour+5*time.Minute), day.Add(11*time.Hour), nil, nil); len(got) != 0 {
		t.Errorf("window at assumed end: got %v, want none", ids(got))
	}
}

func TestClipsOverlapAcrossUTCMidnight(t *testing.T) {
	f := newFixture(t)
	d5 := utc(2026, 11, 5, 0, 0, 0)
	d6 := utc(2026, 11, 6, 0, 0, 0)
	span := f.addClip(clipOpt{Start: d6.Add(-10 * time.Second), Dur: 30 * time.Second}) // 23:59:50 .. 00:00:20
	idx := f.index(f.drv, frontOnly)

	// A window that opens after midnight still sees the clip filed under the previous day.
	if got := mustClips(t, idx, d6, d6.Add(time.Hour), nil, nil); !sameIDs(got, span) {
		t.Errorf("window after midnight: got %v", ids(got))
	}
	if got := mustClips(t, idx, d5, d5.Add(24*time.Hour), nil, nil); !sameIDs(got, span) {
		t.Errorf("window before midnight: got %v", ids(got))
	}
	if got := mustClips(t, idx, d6.Add(20*time.Second), d6.Add(time.Hour), nil, nil); len(got) != 0 {
		t.Errorf("window after the clip: got %v", ids(got))
	}
}

func TestClipsWIBLocalDay(t *testing.T) {
	f := newFixture(t)
	wib := time.FixedZone("WIB", 7*3600)
	// WIB 2026-11-06 = 2026-11-05T17:00Z .. 2026-11-06T17:00Z.
	from := time.Date(2026, 11, 6, 0, 0, 0, 0, wib)
	to := from.AddDate(0, 0, 1)
	prevEvening := f.addClip(clipOpt{Start: utc(2026, 11, 5, 16, 0, 0)}) // 23:00 WIB the day before
	edge := f.addClip(clipOpt{Start: utc(2026, 11, 5, 16, 59, 50)})      // 16:59:50Z + 30 s crosses WIB midnight
	early := f.addClip(clipOpt{Start: utc(2026, 11, 5, 18, 0, 0)})       // 01:00 WIB
	late := f.addClip(clipOpt{Start: utc(2026, 11, 6, 16, 30, 0)})       // 23:30 WIB
	nextDay := f.addClip(clipOpt{Start: utc(2026, 11, 6, 17, 0, 0)})     // 00:00 WIB next day
	idx := f.index(f.drv, frontOnly)

	got := mustClips(t, idx, from, to, nil, nil)
	if !sameIDs(got, edge, early, late) {
		t.Fatalf("WIB day = %v, want [%s %s %s] (not %s / %s)", ids(got), edge.ClipID, early.ClipID, late.ClipID, prevEvening.ClipID, nextDay.ClipID)
	}
}

func TestClipsRangeLimits(t *testing.T) {
	f := newFixture(t)
	idx := f.index(f.drv, frontOnly)
	from := utc(2026, 11, 1, 0, 0, 0)
	ctx := context.Background()

	if _, err := idx.Clips(ctx, from, from.Add(8*24*time.Hour), nil, nil); err != nil {
		t.Errorf("exactly 8 days: %v", err)
	}
	if _, err := idx.Clips(ctx, from, from.Add(8*24*time.Hour+time.Second), nil, nil); !errors.Is(err, ErrRangeTooLarge) {
		t.Errorf("8 days + 1 s: err = %v, want ErrRangeTooLarge", err)
	}
	if _, err := idx.Clips(ctx, from, from, nil, nil); !errors.Is(err, ErrBadRange) {
		t.Errorf("empty range: err = %v, want ErrBadRange", err)
	}
	if _, err := idx.Clips(ctx, from, from.Add(-time.Hour), nil, nil); !errors.Is(err, ErrBadRange) {
		t.Errorf("reversed range: err = %v, want ErrBadRange", err)
	}
}

func TestClipsFiltersSortAndSummaryShape(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	b := f.addClip(clipOpt{Camera: "back", Start: day.Add(2 * time.Hour), Triggers: []string{"person"}})
	a := f.addClip(clipOpt{Camera: "front", Start: day.Add(3 * time.Hour), Triggers: []string{"motion", "vehicle"}})
	c := f.addClip(clipOpt{Camera: "front", Start: day.Add(1 * time.Hour), Triggers: []string{"person"}})
	idx := f.index(f.drv, IndexConfig{CamerasFallback: []string{"front", "back"}})
	from, to := day, day.Add(24*time.Hour)

	if got := mustClips(t, idx, from, to, nil, nil); !sameIDs(got, c, b, a) {
		t.Errorf("sorted by start_time: got %v", ids(got))
	}
	if got := mustClips(t, idx, from, to, []string{"front"}, nil); !sameIDs(got, c, a) {
		t.Errorf("camera=front: got %v", ids(got))
	}
	if got := mustClips(t, idx, from, to, []string{"front", "back"}, []events.Kind{events.KindPerson}); !sameIDs(got, c, b) {
		t.Errorf("trigger=person: got %v", ids(got))
	}
	if got := mustClips(t, idx, from, to, nil, []events.Kind{events.KindVehicle, events.KindPet}); !sameIDs(got, a) {
		t.Errorf("trigger=vehicle|pet: got %v", ids(got))
	}
	if got := mustClips(t, idx, from, to, []string{"../etc", "nope"}, nil); len(got) != 0 {
		t.Errorf("unknown camera filter: got %v", ids(got))
	}

	got := mustClips(t, idx, from, to, []string{"front"}, nil)
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "seek_index") {
		t.Errorf("summary carries seek_index: %s", raw)
	}
	wantHref := `"video_href":"/api/v1/clips/` + c.ClipID.String() + `/video"`
	if !strings.Contains(string(raw), wantHref) {
		t.Errorf("summary lacks %s: %s", wantHref, raw)
	}
	if got[0].Video.Object != c.Video.Object || got[0].Camera.ID != "front" || got[0].VideoHref == "" {
		t.Errorf("summary fields wrong: %+v", got[0])
	}
}

func TestHotDayRefreshAtMostEvery30Seconds(t *testing.T) {
	f := newFixture(t)
	now := testNow
	first := f.addClip(clipOpt{Start: now.Add(-time.Hour)})
	cr := f.counting()
	idx := f.index(cr, frontOnly)
	from, to := utc(2026, 11, 6, 0, 0, 0), utc(2026, 11, 7, 0, 0, 0)

	if got := mustClips(t, idx, from, to, nil, nil); !sameIDs(got, first) {
		t.Fatalf("initial: %v", ids(got))
	}
	base := cr.listCount()
	if base == 0 {
		t.Fatal("first query listed nothing")
	}
	second := f.addClip(clipOpt{Start: now.Add(-30 * time.Minute)})

	f.clk.Advance(29 * time.Second)
	if got := mustClips(t, idx, from, to, nil, nil); !sameIDs(got, first) {
		t.Errorf("within 30 s the cache must serve: %v", ids(got))
	}
	if n := cr.listCount(); n != base {
		t.Errorf("lists after 29 s = %d, want %d", n, base)
	}

	f.clk.Advance(2 * time.Second) // 31 s
	if got := mustClips(t, idx, from, to, nil, nil); !sameIDs(got, first, second) {
		t.Errorf("after 31 s: %v", ids(got))
	}
	if n := cr.listCount(); n <= base {
		t.Errorf("lists after 31 s = %d, want > %d", n, base)
	}
	if n := cr.openCount(sidecarKeyOf(first)); n != 1 {
		t.Errorf("unchanged sidecar read %d times, want 1 (refresh must be incremental)", n)
	}
	if n := cr.openCount(sidecarKeyOf(second)); n != 1 {
		t.Errorf("new sidecar read %d times, want 1", n)
	}

	// The next refresh is measured from the last one.
	after := cr.listCount()
	f.clk.Advance(10 * time.Second)
	mustClips(t, idx, from, to, nil, nil)
	if n := cr.listCount(); n != after {
		t.Errorf("lists 10 s after a refresh = %d, want %d", n, after)
	}
}

func sidecarKeyOf(sc *sidecar.Sidecar) string {
	return sidecar.ClipKeys(sc.Camera.ID, sc.StartTime.Time, sc.ClipID).Sidecar
}

func TestColdDayLRUEvictsLeastRecentlyUsed(t *testing.T) {
	f := newFixture(t)
	days := []time.Time{utc(2026, 10, 1, 8, 0, 0), utc(2026, 10, 2, 8, 0, 0), utc(2026, 10, 3, 8, 0, 0)}
	var clips []*sidecar.Sidecar
	for _, d := range days {
		clips = append(clips, f.addClip(clipOpt{Start: d}))
	}
	cr := f.counting()
	idx := f.index(cr, IndexConfig{CamerasFallback: []string{"front"}, ColdCacheDays: 2})
	ctx := context.Background()
	look := func(i int) {
		t.Helper()
		if _, _, err := idx.Clip(ctx, clips[i].ClipID); err != nil {
			t.Fatalf("Clip %d: %v", i, err)
		}
	}
	prefix := func(i int) string { return sidecar.DayPrefix("front", days[i]) }

	look(0)
	look(1)
	look(2) // evicts day 0, the least recently used
	look(1)
	look(2)
	if n := cr.listsOf(prefix(1)) + cr.listsOf(prefix(2)); n != 2 {
		t.Errorf("cold days 1 and 2 listed %d times, want 2 (cached)", n)
	}
	look(0)
	if n := cr.listsOf(prefix(0)); n != 2 {
		t.Errorf("evicted day listed %d times, want 2", n)
	}
	look(2) // day 1 is now the LRU and goes; day 2 stays
	if n := cr.listsOf(prefix(2)); n != 1 {
		t.Errorf("day 2 listed %d times, want 1", n)
	}
}

func TestColdDayLoadedWhileHotIsReloadedOnce(t *testing.T) {
	f := newFixture(t)
	cr := f.counting()
	idx := f.index(cr, frontOnly)
	d := utc(2026, 11, 6, 22, 0, 0)
	early := f.addClip(clipOpt{Start: d})
	ctx := context.Background()
	if _, _, err := idx.Clip(ctx, early.ClipID); err != nil {
		t.Fatal(err)
	}
	// A clip is committed late, after the day was indexed while hot.
	late := f.addClip(clipOpt{Start: d.Add(time.Hour)})
	f.clk.Advance(48 * time.Hour) // 2026-11-08 12:00: Nov 6 is now cold

	if _, _, err := idx.Clip(ctx, late.ClipID); err != nil {
		t.Fatalf("late clip not found after the day went cold: %v", err)
	}
	n := cr.listsOf(sidecar.DayPrefix("front", d))
	if _, _, err := idx.Clip(ctx, late.ClipID); err != nil {
		t.Fatal(err)
	}
	if got := cr.listsOf(sidecar.DayPrefix("front", d)); got != n {
		t.Errorf("a final cold day was re-listed: %d -> %d", n, got)
	}
}

func TestCameraSetFromStatusWithFallback(t *testing.T) {
	day := utc(2026, 11, 6, 0, 0, 0)
	from, to := day, day.Add(24*time.Hour)
	setup := func(t *testing.T) (*fixture, *sidecar.Sidecar, *sidecar.Sidecar) {
		f := newFixture(t)
		front := f.addClip(clipOpt{Camera: "front", Start: day.Add(time.Hour)})
		back := f.addClip(clipOpt{Camera: "back", Start: day.Add(2 * time.Hour)})
		return f, front, back
	}
	cfg := IndexConfig{CamerasFallback: []string{"back"}}

	t.Run("fresh status wins over fallback", func(t *testing.T) {
		f, front, _ := setup(t)
		f.writeStatus(testNow.Add(-time.Minute), camStatus("front", "up"))
		if got := mustClips(t, f.index(f.drv, cfg), from, to, nil, nil); !sameIDs(got, front) {
			t.Errorf("got %v", ids(got))
		}
	})
	t.Run("missing status uses fallback", func(t *testing.T) {
		f, _, back := setup(t)
		if got := mustClips(t, f.index(f.drv, cfg), from, to, nil, nil); !sameIDs(got, back) {
			t.Errorf("got %v", ids(got))
		}
	})
	t.Run("stale status uses fallback", func(t *testing.T) {
		f, _, back := setup(t)
		f.writeStatus(testNow.Add(-4*time.Minute), camStatus("front", "up"))
		if got := mustClips(t, f.index(f.drv, cfg), from, to, nil, nil); !sameIDs(got, back) {
			t.Errorf("got %v", ids(got))
		}
	})
	t.Run("corrupt status uses fallback", func(t *testing.T) {
		f, _, back := setup(t)
		if err := f.drv.PutSmall(context.Background(), sidecar.StatusKey, []byte("{not json"), "application/json"); err != nil {
			t.Fatal(err)
		}
		if got := mustClips(t, f.index(f.drv, cfg), from, to, nil, nil); !sameIDs(got, back) {
			t.Errorf("got %v", ids(got))
		}
	})
	t.Run("stale status without fallback still names its cameras", func(t *testing.T) {
		f, front, _ := setup(t)
		f.writeStatus(testNow.Add(-time.Hour), camStatus("front", "up"))
		if got := mustClips(t, f.index(f.drv, IndexConfig{}), from, to, nil, nil); !sameIDs(got, front) {
			t.Errorf("got %v", ids(got))
		}
	})
	t.Run("status camera ids that are not valid are never listed", func(t *testing.T) {
		f, _, _ := setup(t)
		f.writeStatus(testNow, camStatus("../etc", "up"), camStatus("front", "up"))
		cr := f.counting()
		mustClips(t, f.index(cr, cfg), from, to, nil, nil)
		for _, p := range cr.lists {
			if strings.Contains(p, "..") || strings.Contains(p, "etc") {
				t.Errorf("listed %q", p)
			}
		}
	})
}

func TestCameraSetChangeReindexesCachedDays(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	front := f.addClip(clipOpt{Camera: "front", Start: day.Add(time.Hour)})
	back := f.addClip(clipOpt{Camera: "back", Start: day.Add(2 * time.Hour)})
	f.writeStatus(testNow, camStatus("front", "up"))
	idx := f.index(f.drv, IndexConfig{})

	if got := mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil); !sameIDs(got, front) {
		t.Fatalf("before: %v", ids(got))
	}
	f.writeStatus(testNow, camStatus("front", "up"), camStatus("back", "up"))
	f.clk.Advance(31 * time.Second)
	if got := mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil); !sameIDs(got, front, back) {
		t.Errorf("after the relay added a camera: %v", ids(got))
	}
}

func TestInvalidSidecarsAreCountedNotServed(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	good := f.addClip(clipOpt{Start: day.Add(1 * time.Hour)})
	other := f.addClip(clipOpt{Start: day.Add(10 * time.Hour)}) // donor id for the mismatch case

	f.addClip(clipOpt{Start: day.Add(2 * time.Hour), Mutate: func(s *sidecar.Sidecar) { s.Schema = "zrec.sidecar/3" }})
	f.addClip(clipOpt{Start: day.Add(3 * time.Hour), NameID: other.ClipID}) // clip_id differs from the filename
	f.addClip(clipOpt{Start: day.Add(4 * time.Hour), NoVideo: true})
	f.addClip(clipOpt{Start: day.Add(5 * time.Hour), Mutate: func(s *sidecar.Sidecar) { s.Video.Object = "sub/" + s.Video.Object }})
	f.addClip(clipOpt{Start: day.Add(6 * time.Hour), Mutate: func(s *sidecar.Sidecar) { s.Video.Object = "../" + s.Video.Object }})
	f.addClip(clipOpt{Start: day.Add(7 * time.Hour), Mutate: func(s *sidecar.Sidecar) { s.Status = "weird" }})
	// Not even JSON.
	junk := sidecar.ClipKeys("front", day.Add(8*time.Hour), f.newID(day.Add(8*time.Hour)))
	if err := f.drv.PutSmall(context.Background(), junk.Sidecar, []byte("{{{"), "application/json"); err != nil {
		t.Fatal(err)
	}
	// A video that never got its sidecar is not a clip and not "invalid".
	orphan := f.addClip(clipOpt{Start: day.Add(9 * time.Hour)})
	f.remove(sidecarKeyOf(orphan))

	idx := f.index(f.drv, frontOnly)
	got := mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil)
	// "other" has a valid sidecar of its own; only the mismatched copy is rejected.
	if !sameIDs(got, good, other) {
		t.Fatalf("served %v, want only the valid clips", ids(got))
	}
	if n := idx.Stats().InvalidSidecars; n != 7 {
		t.Errorf("InvalidSidecars = %d, want 7", n)
	}
	// Invalid clips cannot be fetched either.
	if _, _, err := idx.Clip(context.Background(), other.ClipID); err != nil {
		t.Errorf("valid clip lookup: %v", err)
	}
}

func TestClipLookup(t *testing.T) {
	f := newFixture(t)
	start := utc(2026, 10, 15, 7, 30, 0)
	want := f.addClip(clipOpt{Start: start})
	idx := f.index(f.drv, frontOnly)
	ctx := context.Background()

	sc, videoKey, err := idx.Clip(ctx, want.ClipID)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if sc.ClipID != want.ClipID || sc.Video.SeekIndex == nil || len(*sc.Video.SeekIndex) != 2 {
		t.Errorf("sidecar must be returned in full: %+v", sc)
	}
	keys := sidecar.ClipKeys("front", start, want.ClipID)
	if videoKey != keys.Video {
		t.Errorf("videoKey = %q, want %q", videoKey, keys.Video)
	}
	if _, err := f.drv.Stat(ctx, videoKey); err != nil {
		t.Errorf("videoKey does not resolve: %v", err)
	}

	missing := f.newID(start)
	if _, _, err := idx.Clip(ctx, missing); !errors.Is(err, ErrClipNotFound) {
		t.Errorf("unknown id: err = %v, want ErrClipNotFound", err)
	}
	other := f.newID(start.Add(-48 * time.Hour))
	if _, _, err := idx.Clip(ctx, other); !errors.Is(err, ErrClipNotFound) {
		t.Errorf("id from an empty day: err = %v, want ErrClipNotFound", err)
	}
}

func TestSeriesNextAcrossMidnight(t *testing.T) {
	f := newFixture(t)
	d6 := utc(2026, 11, 6, 0, 0, 0)
	seg0 := f.addClip(clipOpt{Start: d6.Add(-20 * time.Second), Dur: 20 * time.Second}) // 23:59:40, ends at midnight
	seg1 := f.addClip(clipOpt{Start: d6, Dur: 30 * time.Second, Series: seg0.SeriesID, Segment: 1})
	seg2 := f.addClip(clipOpt{Start: d6.Add(30 * time.Second), Series: seg0.SeriesID, Segment: 2})
	// A different camera's clip with the same series id and index must not match.
	f.addClip(clipOpt{Camera: "back", Start: d6, Series: seg0.SeriesID, Segment: 1})
	idx := f.index(f.drv, IndexConfig{CamerasFallback: []string{"front", "back"}})
	ctx := context.Background()

	next, err := idx.NextInSeries(ctx, seg0)
	if err != nil {
		t.Fatalf("NextInSeries: %v", err)
	}
	if next == nil || next.ClipID != seg1.ClipID || next.Camera.ID != "front" {
		t.Fatalf("next of segment 0 = %+v, want %s", next, seg1.ClipID)
	}
	if next.VideoHref == "" {
		t.Error("next has no video_href")
	}
	next, err = idx.NextInSeries(ctx, seg1)
	if err != nil || next == nil || next.ClipID != seg2.ClipID {
		t.Fatalf("next of segment 1 = %+v, %v", next, err)
	}
	next, err = idx.NextInSeries(ctx, seg2)
	if err != nil || next != nil {
		t.Fatalf("next of the last segment = %+v, %v; want nil, nil", next, err)
	}
}

func TestRetentionInvalidatesColdDays(t *testing.T) {
	f := newFixture(t)
	old := utc(2026, 9, 20, 8, 0, 0)     // 47 days before the clock: past retention
	recent := utc(2026, 10, 20, 8, 0, 0) // 17 days: inside retention
	oldClip := f.addClip(clipOpt{Start: old})
	recentClip := f.addClip(clipOpt{Start: recent})
	cr := f.counting()
	idx := f.index(cr, IndexConfig{CamerasFallback: []string{"front"}, RetentionDays: 30})
	ctx := context.Background()

	span := func(d time.Time) []Summary {
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		return mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil)
	}
	if len(span(old)) != 1 || len(span(recent)) != 1 {
		t.Fatal("setup: clips not indexed")
	}
	// ILM removes both days' objects.
	for _, c := range []*sidecar.Sidecar{oldClip, recentClip} {
		f.remove(sidecarKeyOf(c))
		f.remove(sidecar.ClipKeys("front", c.StartTime.Time, c.ClipID).Video)
	}

	// Inside the refresh interval, nothing is re-listed for either day.
	f.clk.Advance(10 * time.Second)
	if len(span(old)) != 1 {
		t.Error("expired day dropped before the refresh interval")
	}
	f.clk.Advance(25 * time.Second)
	if got := span(old); len(got) != 0 {
		t.Errorf("day past retention_days still served after re-list: %v", ids(got))
	}
	if _, _, err := idx.Clip(ctx, oldClip.ClipID); !errors.Is(err, ErrClipNotFound) {
		t.Errorf("expired clip lookup: err = %v, want ErrClipNotFound", err)
	}
	// A day inside the retention window is a normal cold day: stale entries are harmless.
	if got := span(recent); len(got) != 1 {
		t.Errorf("day inside retention was invalidated: %v", ids(got))
	}
	// The expired day's re-list is rate limited like a hot day.
	n := cr.listsOf(sidecar.DayPrefix("front", old))
	span(old)
	if got := cr.listsOf(sidecar.DayPrefix("front", old)); got != n {
		t.Errorf("expired day re-listed %d -> %d within the refresh interval", n, got)
	}
}

func TestRetentionDisabledWhenZero(t *testing.T) {
	f := newFixture(t)
	old := utc(2026, 1, 5, 8, 0, 0)
	c := f.addClip(clipOpt{Start: old})
	idx := f.index(f.drv, frontOnly)
	day := utc(2026, 1, 5, 0, 0, 0)
	if got := mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil); len(got) != 1 {
		t.Fatal("setup")
	}
	f.remove(sidecarKeyOf(c))
	f.clk.Advance(time.Hour)
	if got := mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil); len(got) != 1 {
		t.Errorf("retention_days 0 must keep the cold cache: %v", ids(got))
	}
}

func TestCamerasMergesStatusAndIndexedSidecars(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	frontLast := f.addClip(clipOpt{Camera: "front", Start: day.Add(3 * time.Hour), Mutate: func(s *sidecar.Sidecar) {
		s.Camera.Brand = "tapo"
		s.Camera.Tags = []string{"exterior"}
	}})
	f.addClip(clipOpt{Camera: "front", Start: day.Add(1 * time.Hour)})
	f.addClip(clipOpt{Camera: "garage", Start: day.Add(2 * time.Hour)})
	f.writeStatus(testNow.Add(-30*time.Second), camStatus("front", "up"), camStatus("porch", "down"))
	idx := f.index(f.drv, IndexConfig{CamerasFallback: []string{"garage"}})
	ctx := context.Background()

	// Cameras seen only in sidecars appear once their day is indexed. The
	// status lists front and porch, so the fallback (garage) is not listed.
	mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil)
	got, err := idx.Cameras(ctx)
	if err != nil {
		t.Fatalf("Cameras: %v", err)
	}
	byID := map[string]CameraSummary{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if len(got) != 2 || got[0].ID != "front" || got[1].ID != "porch" {
		t.Fatalf("cameras = %+v, want front and porch sorted", got)
	}
	fr := byID["front"]
	if !fr.Online || fr.Brand != "tapo" || len(fr.Tags) != 1 || fr.Tags[0] != "exterior" {
		t.Errorf("front = %+v", fr)
	}
	if fr.LastClipAt == nil || !fr.LastClipAt.Equal(frontLast.StartTime.Time) {
		t.Errorf("front last_clip_at = %v, want %v", fr.LastClipAt, frontLast.StartTime)
	}
	if p := byID["porch"]; p.Online || p.LastClipAt != nil || p.Tags == nil {
		t.Errorf("porch = %+v (down camera, no clips, tags must be [] not null)", p)
	}

	// A stale status marks every camera offline.
	f.clk.Advance(5 * time.Minute)
	got, err = idx.Cameras(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Online {
			t.Errorf("%s online with a stale status", c.ID)
		}
	}
}

func TestCamerasFallbackOnly(t *testing.T) {
	f := newFixture(t)
	idx := f.index(f.drv, IndexConfig{CamerasFallback: []string{"zeta", "alpha"}})
	got, err := idx.Cameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "zeta" || got[0].Online {
		t.Errorf("cameras = %+v", got)
	}
}

func TestStats(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	f.addClip(clipOpt{Start: day.Add(time.Hour)})
	f.addClip(clipOpt{Start: day.Add(2 * time.Hour), NoVideo: true})
	idx := f.index(f.drv, frontOnly)

	if s := idx.Stats(); s.IndexedDays != 0 || s.InvalidSidecars != 0 || !s.IndexFreshAt.IsZero() {
		t.Errorf("fresh index stats = %+v", s)
	}
	mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil)
	s := idx.Stats()
	if s.IndexedDays < 1 || s.InvalidSidecars != 1 || !s.IndexFreshAt.Equal(testNow) {
		t.Errorf("stats = %+v", s)
	}
	raw, _ := json.Marshal(s)
	for _, k := range []string{`"indexed_days"`, `"invalid_sidecars"`, `"index_fresh_at"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("stats json lacks %s: %s", k, raw)
		}
	}
	// A refresh must not double count the same invalid sidecar.
	f.clk.Advance(time.Minute)
	mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil)
	if n := idx.Stats().InvalidSidecars; n != 1 {
		t.Errorf("InvalidSidecars after refresh = %d, want 1", n)
	}
}

func TestListErrorIsReturnedAndNotCached(t *testing.T) {
	f := newFixture(t)
	day := utc(2026, 11, 6, 0, 0, 0)
	c := f.addClip(clipOpt{Start: day.Add(time.Hour)})
	cr := f.counting()
	idx := f.index(cr, frontOnly)

	cr.failList.Store(true)
	if _, err := idx.Clips(context.Background(), day, day.Add(24*time.Hour), nil, nil); err == nil {
		t.Fatal("storage failure was swallowed")
	}
	cr.failList.Store(false)
	if got := mustClips(t, idx, day, day.Add(24*time.Hour), nil, nil); !sameIDs(got, c) {
		t.Errorf("after recovery: %v", ids(got))
	}
}

func TestFutureDaysDoNotTouchStorage(t *testing.T) {
	f := newFixture(t)
	cr := f.counting()
	idx := f.index(cr, frontOnly)
	from := utc(2026, 12, 1, 0, 0, 0)
	got := mustClips(t, idx, from, from.Add(24*time.Hour), nil, nil)
	if len(got) != 0 || got == nil {
		t.Errorf("got %v, want empty non-nil", got)
	}
	if n := cr.listCount(); n != 0 {
		t.Errorf("listed %d prefixes for days that cannot hold clips yet", n)
	}
}
