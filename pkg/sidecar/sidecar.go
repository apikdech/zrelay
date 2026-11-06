// Package sidecar defines the plaintext metadata schemas stored next to
// encrypted clips (zrec.sidecar/2), the relay status and coverage
// documents (zrec.status/1, zrec.coverage/1) and the storage key layout
// (Contract §6 and §7).
package sidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

// Schema is the sidecar schema this package writes and validates.
const Schema = "zrec.sidecar/2"

const (
	schemaPrefix = "zrec.sidecar/"
	schemaMajor  = 2
	maxStringLen = 128
	maxTags      = 16
)

// Sidecar statuses.
const (
	StatusComplete   = "complete"
	StatusIncomplete = "incomplete"
)

// ErrUnknownSchema is returned by Decode for a schema other than
// zrec.sidecar/2.
var ErrUnknownSchema = errors.New("sidecar: unknown schema")

// FieldError reports a validation failure of one field, named by its JSON
// path (for example "camera.tags[3]" or "video.seek_index").
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return "sidecar: " + e.Field + ": " + e.Reason }

func fieldErr(field, format string, args ...any) error {
	return &FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

var (
	// cameraIDRE is the header camera_id pattern (Contract §4.1).
	cameraIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	keyIDRE    = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

var (
	endReasons   = set("post_roll_elapsed", "max_duration", "stream_lost", "codec_change", "backlog_overflow", "shutdown", "recovered")
	triggerTypes = set("motion", "person", "vehicle", "pet", "other")
	timeSources  = set("rtcp", "arrival")
)

func set(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// Sidecar is the zrec.sidecar/2 document (Contract §6.1). Nullable fields
// are pointers.
type Sidecar struct {
	Schema       string    `json:"schema"`
	Status       string    `json:"status"`
	ClipID       ulid.ULID `json:"clip_id"`
	SeriesID     ulid.ULID `json:"series_id"`
	SegmentIndex int       `json:"segment_index"`
	Camera       Camera    `json:"camera"`
	StartTime    Time      `json:"start_time"`
	EndTime      *Time     `json:"end_time"`
	DurationMs   *int64    `json:"duration_ms"`
	PreRollMs    int64     `json:"pre_roll_ms"`
	EndReason    string    `json:"end_reason"`
	Triggers     []Trigger `json:"triggers"`
	Video        Video     `json:"video"`
	Recorder     Recorder  `json:"recorder"`
}

// Camera identifies the source camera. Name, Brand and Tags are untrusted
// display strings.
type Camera struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Brand string   `json:"brand"`
	Tags  []string `json:"tags"`
}

// Trigger is one event type that fired during the clip. Offsets are from
// the clip start.
type Trigger struct {
	Type      string   `json:"type"`
	FirstAtMs int64    `json:"first_at_ms"`
	LastAtMs  int64    `json:"last_at_ms"`
	RawTopics []string `json:"raw_topics,omitempty"`
}

// Video describes the encrypted video object, a sibling of the sidecar.
type Video struct {
	Object             string      `json:"object"`
	FormatVersion      int         `json:"format_version"`
	KeyID              string      `json:"key_id"`
	ChunkSize          int64       `json:"chunk_size"`
	SizeBytes          int64       `json:"size_bytes"`
	PlaintextSizeBytes *int64      `json:"plaintext_size_bytes"`
	Container          string      `json:"container"`
	Codec              string      `json:"codec"`
	Width              int         `json:"width"`
	Height             int         `json:"height"`
	InitSegmentBytes   int64       `json:"init_segment_bytes"`
	SeekIndex          *[][2]int64 `json:"seek_index"`
}

// Recorder identifies the relay that wrote the clip.
type Recorder struct {
	RelayID    string `json:"relay_id"`
	Version    string `json:"version"`
	TimeSource string `json:"time_source"`
}

// Decode parses a sidecar. It rejects any schema other than
// zrec.sidecar/2 with ErrUnknownSchema and ignores unknown fields. It does
// not validate the content; call Validate.
func Decode(b []byte) (*Sidecar, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, fmt.Errorf("sidecar: decode: %w", err)
	}
	if err := checkSchema(head.Schema); err != nil {
		return nil, err
	}
	var sc Sidecar
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, fmt.Errorf("sidecar: decode: %w", err)
	}
	return &sc, nil
}

func checkSchema(s string) error {
	major, ok := strings.CutPrefix(s, schemaPrefix)
	n, err := strconv.Atoi(major)
	if !ok || err != nil || n != schemaMajor || strconv.Itoa(n) != major {
		return fmt.Errorf("%w: schema %q", ErrUnknownSchema, s)
	}
	return nil
}

// Validate checks every Contract §6.2 rule. Required fields are required
// for incomplete (recovered) sidecars too (Contract §6.3). It returns the
// first violation as a *FieldError.
func (s *Sidecar) Validate() error {
	if err := checkSchema(s.Schema); err != nil {
		return fieldErr("schema", "%v", err)
	}
	complete := false
	switch s.Status {
	case StatusComplete:
		complete = true
	case StatusIncomplete:
	default:
		return fieldErr("status", "must be complete or incomplete, got %q", s.Status)
	}
	if s.ClipID == (ulid.ULID{}) {
		return fieldErr("clip_id", "required")
	}
	if s.SeriesID == (ulid.ULID{}) {
		return fieldErr("series_id", "required")
	}
	if s.SegmentIndex < 0 {
		return fieldErr("segment_index", "must be >= 0")
	}
	if err := s.Camera.validate(); err != nil {
		return err
	}
	if s.StartTime.IsZero() {
		return fieldErr("start_time", "required")
	}
	if s.EndTime != nil && s.EndTime.Before(s.StartTime.Time) {
		return fieldErr("end_time", "before start_time")
	}
	if complete && s.EndTime == nil {
		return fieldErr("end_time", "required when complete")
	}
	if s.DurationMs != nil && *s.DurationMs < 0 {
		return fieldErr("duration_ms", "must be >= 0")
	}
	if complete && s.DurationMs == nil {
		return fieldErr("duration_ms", "required when complete")
	}
	if s.PreRollMs < 0 {
		return fieldErr("pre_roll_ms", "must be >= 0")
	}
	if s.SegmentIndex > 0 && s.PreRollMs != 0 {
		return fieldErr("pre_roll_ms", "must be 0 for continuation segment %d", s.SegmentIndex)
	}
	if !endReasons[s.EndReason] {
		return fieldErr("end_reason", "unknown value %q", s.EndReason)
	}
	// Only crash recovery produces incomplete sidecars (Contract §7.4).
	if (s.EndReason == "recovered") == complete {
		return fieldErr("end_reason", "%q is inconsistent with status %q", s.EndReason, s.Status)
	}
	if s.Triggers == nil {
		return fieldErr("triggers", "required (may be empty)")
	}
	for i, tr := range s.Triggers {
		p := fmt.Sprintf("triggers[%d]", i)
		if !triggerTypes[tr.Type] {
			return fieldErr(p+".type", "unknown value %q", tr.Type)
		}
		if tr.FirstAtMs < 0 {
			return fieldErr(p+".first_at_ms", "must be >= 0")
		}
		if tr.LastAtMs < tr.FirstAtMs {
			return fieldErr(p+".last_at_ms", "before first_at_ms")
		}
	}
	if err := s.Video.validate(complete); err != nil {
		return err
	}
	if !timeSources[s.Recorder.TimeSource] {
		return fieldErr("recorder.time_source", "unknown value %q", s.Recorder.TimeSource)
	}
	return nil
}

func (c *Camera) validate() error {
	if !cameraIDRE.MatchString(c.ID) {
		return fieldErr("camera.id", "must match %s", cameraIDRE)
	}
	if err := checkLen("camera.name", c.Name); err != nil {
		return err
	}
	if err := checkLen("camera.brand", c.Brand); err != nil {
		return err
	}
	if c.Tags == nil {
		return fieldErr("camera.tags", "required (may be empty)")
	}
	if len(c.Tags) > maxTags {
		return fieldErr("camera.tags", "%d tags, max %d", len(c.Tags), maxTags)
	}
	for i, tag := range c.Tags {
		if err := checkLen(fmt.Sprintf("camera.tags[%d]", i), tag); err != nil {
			return err
		}
	}
	return nil
}

func checkLen(field, s string) error {
	if n := utf8.RuneCountInString(s); n > maxStringLen {
		return fieldErr(field, "%d characters, max %d", n, maxStringLen)
	}
	return nil
}

func (v *Video) validate(complete bool) error {
	if !NameRE.MatchString(v.Object) || !strings.HasSuffix(v.Object, ".zrec") {
		return fieldErr("video.object", "must be a bare HHMMSSmmm_<ULID>.zrec name, got %q", v.Object)
	}
	if v.FormatVersion < 1 {
		return fieldErr("video.format_version", "must be >= 1")
	}
	if !keyIDRE.MatchString(v.KeyID) {
		return fieldErr("video.key_id", "must be 16 lowercase hex digits")
	}
	if v.ChunkSize <= 0 {
		return fieldErr("video.chunk_size", "must be > 0")
	}
	if v.SizeBytes <= 0 {
		return fieldErr("video.size_bytes", "must be > 0")
	}
	if v.PlaintextSizeBytes != nil && *v.PlaintextSizeBytes < 0 {
		return fieldErr("video.plaintext_size_bytes", "must be >= 0")
	}
	if complete && v.PlaintextSizeBytes == nil {
		return fieldErr("video.plaintext_size_bytes", "required when complete")
	}
	if v.Container == "" {
		return fieldErr("video.container", "required")
	}
	if v.Codec == "" {
		return fieldErr("video.codec", "required")
	}
	if v.Width < 0 {
		return fieldErr("video.width", "must be >= 0")
	}
	if v.Height < 0 {
		return fieldErr("video.height", "must be >= 0")
	}
	if v.InitSegmentBytes <= 0 {
		return fieldErr("video.init_segment_bytes", "must be > 0")
	}
	if complete && v.SeekIndex == nil {
		return fieldErr("video.seek_index", "required when complete")
	}
	if v.SeekIndex != nil {
		var prev [2]int64
		for i, e := range *v.SeekIndex {
			if e[0] < 0 || e[1] < 0 {
				return fieldErr("video.seek_index", "entry %d is negative", i)
			}
			if i > 0 && (e[0] < prev[0] || e[1] <= prev[1]) {
				return fieldErr("video.seek_index", "entry %d is not ascending", i)
			}
			prev = e
		}
	}
	return nil
}
