package status_test

import (
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/status"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
)

func utc(h, m, s int) time.Time { return time.Date(2026, 11, 6, h, m, s, 0, time.UTC) }

func hourStart(h int) time.Time { return utc(h, 0, 0) }

func wantIvs(t *testing.T, c sidecar.Coverage, want ...[2]time.Time) {
	t.Helper()
	if len(c.Ready) != len(want) {
		t.Fatalf("ready = %v, want %v", c.Ready, want)
	}
	for i := range want {
		if !c.Ready[i][0].Equal(want[i][0]) || !c.Ready[i][1].Equal(want[i][1]) {
			t.Fatalf("ready[%d] = %v, want %v", i, c.Ready[i], want[i])
		}
	}
}

func TestTrackerClampsIntervalsToTheHour(t *testing.T) {
	clk := clock.NewFake(utc(13, 30, 0))
	tr := status.NewCoverageTracker(clk)
	tr.SetReady(true, utc(13, 30, 0))
	clk.Advance(110 * time.Minute) // 15:20, the interval is still open

	h13 := tr.TakeHour(hourStart(13))
	wantIvs(t, h13, [2]time.Time{utc(13, 30, 0), utc(14, 0, 0)})
	h14 := tr.TakeHour(hourStart(14))
	wantIvs(t, h14, [2]time.Time{utc(14, 0, 0), utc(15, 0, 0)})
	h15 := tr.TakeHour(hourStart(15))
	wantIvs(t, h15, [2]time.Time{utc(15, 0, 0), utc(15, 20, 0)})

	for _, c := range []sidecar.Coverage{h13, h14, h15} {
		if c.Schema != sidecar.CoverageSchema {
			t.Fatalf("schema = %q", c.Schema)
		}
		c.CameraID = "front-door"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	}
}

func TestTrackerMergesTouchingAndIgnoresDuplicateTransitions(t *testing.T) {
	clk := clock.NewFake(utc(14, 0, 0))
	tr := status.NewCoverageTracker(clk)
	tr.SetReady(true, utc(14, 0, 0))
	tr.SetReady(true, utc(14, 5, 0)) // already ready: no-op
	tr.SetReady(false, utc(14, 10, 0))
	tr.SetReady(false, utc(14, 10, 0)) // already not ready: no-op
	tr.SetReady(true, utc(14, 10, 0))  // touches the previous interval: merged
	tr.SetReady(false, utc(14, 20, 0))
	tr.SetReady(true, utc(14, 30, 0))
	tr.SetReady(false, utc(14, 30, 0)) // zero length: discarded
	tr.SetReady(true, utc(14, 40, 0))
	tr.SetReady(false, utc(14, 50, 0))
	clk.Advance(time.Hour)

	c := tr.TakeHour(hourStart(14))
	wantIvs(t, c,
		[2]time.Time{utc(14, 0, 0), utc(14, 20, 0)},
		[2]time.Time{utc(14, 40, 0), utc(14, 50, 0)})
}

func TestTrackerDropsAreAssignedToTheirHourAndFormJSONArrays(t *testing.T) {
	clk := clock.NewFake(utc(14, 0, 0))
	tr := status.NewCoverageTracker(clk)
	tr.Drop(utc(14, 37, 10), "stream_lost", 52000)
	tr.Drop(utc(15, 1, 0), "backlog", 9000)
	clk.Advance(2 * time.Hour)

	c14 := tr.TakeHour(hourStart(14))
	if len(c14.Dropped) != 1 || c14.Dropped[0].Reason != "stream_lost" || c14.Dropped[0].ApproxMs != 52000 ||
		!c14.Dropped[0].At.Equal(utc(14, 37, 10)) {
		t.Fatalf("hour 14 dropped = %+v", c14.Dropped)
	}
	c15 := tr.TakeHour(hourStart(15))
	if len(c15.Dropped) != 1 || c15.Dropped[0].Reason != "backlog" {
		t.Fatalf("hour 15 dropped = %+v", c15.Dropped)
	}
	empty := tr.TakeHour(hourStart(16))
	if empty.Ready == nil || empty.Dropped == nil {
		t.Fatal("empty hour must carry non-nil ready and dropped (Contract 7.5.2: empty arrays, not null)")
	}
}

func TestTrackerTakeIsDestructive(t *testing.T) {
	clk := clock.NewFake(utc(14, 0, 0))
	tr := status.NewCoverageTracker(clk)
	tr.SetReady(true, utc(14, 0, 0))
	tr.SetReady(false, utc(14, 10, 0))
	tr.Drop(utc(14, 10, 0), "stream_lost", 1)
	clk.Advance(time.Hour)

	if c := tr.TakeHour(hourStart(14)); len(c.Ready) != 1 || len(c.Dropped) != 1 {
		t.Fatalf("first take = %+v", c)
	}
	if c := tr.TakeHour(hourStart(14)); len(c.Ready) != 0 || len(c.Dropped) != 0 {
		t.Fatalf("second take = %+v, want empty (memory is released)", c)
	}
}
