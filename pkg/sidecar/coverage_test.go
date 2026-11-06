package sidecar

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func at(h, m, s int) Time { return Time{time.Date(2026, 9, 16, h, m, s, 0, time.UTC)} }

func validCoverage() Coverage {
	return Coverage{
		Schema:    CoverageSchema,
		CameraID:  "front-door",
		HourStart: at(14, 0, 0),
		Ready: []Interval{
			{at(14, 0, 0), at(14, 37, 10)},
			{at(14, 38, 2), at(15, 0, 0)},
		},
		Dropped: []Dropped{{At: at(14, 37, 10), Reason: "stream_lost", ApproxMs: 52000}},
	}
}

func TestCoverageValidate(t *testing.T) {
	c := validCoverage()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid coverage rejected: %v", err)
	}

	empty := Coverage{Schema: CoverageSchema, CameraID: "front-door", HourStart: at(14, 0, 0),
		Ready: []Interval{}, Dropped: []Dropped{}}
	if err := empty.Validate(); err != nil {
		t.Errorf("empty ready/dropped rejected: %v", err)
	}
	touching := validCoverage()
	touching.Ready = []Interval{{at(14, 0, 0), at(14, 10, 0)}, {at(14, 10, 0), at(14, 20, 0)}}
	if err := touching.Validate(); err != nil {
		t.Errorf("touching intervals rejected: %v", err)
	}

	cases := map[string]struct {
		mut   func(*Coverage)
		field string
	}{
		"schema":         {func(c *Coverage) { c.Schema = "zrec.coverage/2" }, "schema"},
		"camera_id":      {func(c *Coverage) { c.CameraID = "Front_Door" }, "camera_id"},
		"hour_start":     {func(c *Coverage) { c.HourStart = at(14, 0, 1) }, "hour_start"},
		"hour_start_ms":  {func(c *Coverage) { c.HourStart = Time{at(14, 0, 0).Add(time.Millisecond)} }, "hour_start"},
		"ready_nil":      {func(c *Coverage) { c.Ready = nil }, "ready"},
		"dropped_nil":    {func(c *Coverage) { c.Dropped = nil }, "dropped"},
		"reversed":       {func(c *Coverage) { c.Ready[0] = Interval{at(14, 37, 10), at(14, 0, 0)} }, "ready[0]"},
		"empty_interval": {func(c *Coverage) { c.Ready[0] = Interval{at(14, 5, 0), at(14, 5, 0)} }, "ready[0]"},
		"before_hour":    {func(c *Coverage) { c.Ready[0][0] = at(13, 59, 59) }, "ready[0]"},
		"after_hour":     {func(c *Coverage) { c.Ready[1][1] = at(15, 0, 1) }, "ready[1]"},
		"overlap": {func(c *Coverage) {
			c.Ready = []Interval{{at(14, 0, 0), at(14, 30, 0)}, {at(14, 29, 59), at(14, 40, 0)}}
		}, "ready[1]"},
		"descending": {func(c *Coverage) {
			c.Ready = []Interval{{at(14, 30, 0), at(14, 40, 0)}, {at(14, 0, 0), at(14, 10, 0)}}
		}, "ready[1]"},
		"drop_reason": {func(c *Coverage) { c.Dropped[0].Reason = "max_duration" }, "dropped[0].reason"},
		"drop_approx": {func(c *Coverage) { c.Dropped[0].ApproxMs = -1 }, "dropped[0].approx_ms"},
		"drop_at":     {func(c *Coverage) { c.Dropped[0].At = Time{} }, "dropped[0].at"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCoverage()
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("accepted invalid coverage")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name %q", err, tc.field)
			}
		})
	}
}

func TestCoverageJSONShape(t *testing.T) {
	in := `{
  "schema": "zrec.coverage/1",
  "camera_id": "front-door",
  "hour_start": "2026-09-16T14:00:00Z",
  "ready": [["2026-09-16T14:00:00Z", "2026-09-16T14:37:10Z"],
            ["2026-09-16T14:38:02Z", "2026-09-16T15:00:00Z"]],
  "dropped": [{ "at": "2026-09-16T14:37:10Z", "reason": "stream_lost", "approx_ms": 52000 }]
}`
	var c Coverage
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	want := validCoverage()
	if len(c.Ready) != 2 || !c.Ready[1][0].Equal(want.Ready[1][0].Time) || c.Dropped[0] != want.Dropped[0] {
		t.Errorf("decoded %+v", c)
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ready":[["2026-09-16T14:00:00.000Z","2026-09-16T14:37:10.000Z"]`) {
		t.Errorf("marshaled %s", b)
	}
}
