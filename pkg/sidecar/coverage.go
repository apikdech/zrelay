package sidecar

import (
	"fmt"
	"time"
)

// CoverageSchema is the schema of hourly coverage objects (Contract §7.5.2).
const CoverageSchema = "zrec.coverage/1"

var dropReasons = set("stream_lost", "backlog", "shutdown")

// Coverage records when one camera could have captured an event during one
// UTC hour (Contract §7.5.2).
type Coverage struct {
	Schema    string     `json:"schema"`
	CameraID  string     `json:"camera_id"`
	HourStart Time       `json:"hour_start"`
	Ready     []Interval `json:"ready"`
	Dropped   []Dropped  `json:"dropped"`
}

// Interval is a [start, end] pair of UTC times.
type Interval [2]Time

// Dropped records footage lost during the hour.
type Dropped struct {
	At       Time   `json:"at"`
	Reason   string `json:"reason"` // stream_lost | backlog | shutdown
	ApproxMs int64  `json:"approx_ms"`
}

// Validate checks the Contract §7.5.2 rules: ready intervals are
// non-empty, ascending, non-overlapping and clamped to the hour. Ready and
// Dropped may be empty but not nil (null).
func (c *Coverage) Validate() error {
	if c.Schema != CoverageSchema {
		return fieldErr("schema", "must be %q, got %q", CoverageSchema, c.Schema)
	}
	if !cameraIDRE.MatchString(c.CameraID) {
		return fieldErr("camera_id", "must match %s", cameraIDRE)
	}
	h := c.HourStart.UTC()
	if h.IsZero() || !h.Truncate(time.Hour).Equal(h) {
		return fieldErr("hour_start", "must be exactly on the hour, got %s", h.Format(time.RFC3339Nano))
	}
	end := h.Add(time.Hour)
	if c.Ready == nil {
		return fieldErr("ready", "required (may be empty)")
	}
	for i, iv := range c.Ready {
		p := fmt.Sprintf("ready[%d]", i)
		if !iv[0].Before(iv[1].Time) {
			return fieldErr(p, "start is not before end")
		}
		if iv[0].Before(h) || iv[1].After(end) {
			return fieldErr(p, "not clamped to the hour")
		}
		if i > 0 && iv[0].Before(c.Ready[i-1][1].Time) {
			return fieldErr(p, "overlaps or precedes ready[%d]", i-1)
		}
	}
	if c.Dropped == nil {
		return fieldErr("dropped", "required (may be empty)")
	}
	for i, d := range c.Dropped {
		p := fmt.Sprintf("dropped[%d]", i)
		if d.At.IsZero() {
			return fieldErr(p+".at", "required")
		}
		if !dropReasons[d.Reason] {
			return fieldErr(p+".reason", "unknown value %q", d.Reason)
		}
		if d.ApproxMs < 0 {
			return fieldErr(p+".approx_ms", "must be >= 0")
		}
	}
	return nil
}
