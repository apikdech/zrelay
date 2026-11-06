package sidecar

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

const fixtureDir = "../../testdata/sidecars"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeValidFixtures(t *testing.T) {
	for _, name := range []string{"valid_complete.json", "valid_incomplete.json"} {
		t.Run(name, func(t *testing.T) {
			sc, err := Decode(readFixture(t, name))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if err := sc.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if sc.ClipID != ulid.MustParse("01M2NBMVS0K8EKRQKGGAJCDT0B") {
				t.Errorf("clip_id = %s", sc.ClipID)
			}
			if sc.Camera.ID != "front-door" {
				t.Errorf("camera.id = %q", sc.Camera.ID)
			}
			want := time.Date(2026, 9, 16, 14, 59, 0, 0, time.UTC)
			if !sc.StartTime.Equal(want) {
				t.Errorf("start_time = %v, want %v", sc.StartTime, want)
			}
		})
	}

	sc, err := Decode(readFixture(t, "valid_complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Status != StatusComplete || sc.EndReason != "post_roll_elapsed" {
		t.Errorf("status/end_reason = %q/%q", sc.Status, sc.EndReason)
	}
	if sc.EndTime == nil || !sc.EndTime.Equal(time.Date(2026, 9, 16, 14, 59, 42, 500e6, time.UTC)) {
		t.Errorf("end_time = %v", sc.EndTime)
	}
	if sc.DurationMs == nil || *sc.DurationMs != 42500 {
		t.Errorf("duration_ms = %v", sc.DurationMs)
	}
	if sc.Video.PlaintextSizeBytes == nil || *sc.Video.PlaintextSizeBytes != 16777000 {
		t.Errorf("plaintext_size_bytes = %v", sc.Video.PlaintextSizeBytes)
	}
	if sc.Video.SeekIndex == nil || len(*sc.Video.SeekIndex) != 3 || (*sc.Video.SeekIndex)[1] != [2]int64{2000, 791344} {
		t.Errorf("seek_index = %v", sc.Video.SeekIndex)
	}
	if len(sc.Triggers) != 2 || sc.Triggers[1].Type != "person" || sc.Triggers[1].FirstAtMs != 16900 ||
		len(sc.Triggers[0].RawTopics) != 1 {
		t.Errorf("triggers = %+v", sc.Triggers)
	}
	if sc.Recorder.TimeSource != "rtcp" || sc.Video.KeyID != "a1b2c3d4e5f60718" || sc.Video.InitSegmentBytes != 1187 {
		t.Errorf("recorder/video = %+v %+v", sc.Recorder, sc.Video)
	}

	inc, err := Decode(readFixture(t, "valid_incomplete.json"))
	if err != nil {
		t.Fatal(err)
	}
	if inc.Status != StatusIncomplete || inc.EndTime != nil || inc.DurationMs != nil ||
		inc.Video.PlaintextSizeBytes != nil || inc.Video.SeekIndex != nil {
		t.Errorf("incomplete nullable fields not nil: %+v", inc)
	}
}

// invalidCases maps every invalid fixture to the field its error must name.
var invalidCases = map[string]string{
	"invalid_schema_major.json":                 "schema",
	"invalid_schema_name.json":                  "schema",
	"invalid_status.json":                       "status",
	"invalid_clip_id_missing.json":              "clip_id",
	"invalid_series_id_missing.json":            "series_id",
	"invalid_segment_index_negative.json":       "segment_index",
	"invalid_camera_id_pattern.json":            "camera.id",
	"invalid_camera_name_long.json":             "camera.name",
	"invalid_camera_brand_long.json":            "camera.brand",
	"invalid_camera_tags_missing.json":          "camera.tags",
	"invalid_camera_tags_count.json":            "camera.tags",
	"invalid_camera_tag_long.json":              "camera.tags[1]",
	"invalid_start_time_missing.json":           "start_time",
	"invalid_complete_end_time_null.json":       "end_time",
	"invalid_end_time_before_start.json":        "end_time",
	"invalid_complete_duration_null.json":       "duration_ms",
	"invalid_duration_negative.json":            "duration_ms",
	"invalid_pre_roll_negative.json":            "pre_roll_ms",
	"invalid_pre_roll_continuation.json":        "pre_roll_ms",
	"invalid_end_reason.json":                   "end_reason",
	"invalid_complete_end_reason_recovered.json": "end_reason",
	"invalid_triggers_missing.json":             "triggers",
	"invalid_trigger_type.json":                 "triggers[0].type",
	"invalid_trigger_offset_negative.json":      "triggers[0].first_at_ms",
	"invalid_trigger_last_before_first.json":    "triggers[0].last_at_ms",
	"invalid_video_object_path.json":            "video.object",
	"invalid_video_object_ext.json":             "video.object",
	"invalid_video_format_version.json":         "video.format_version",
	"invalid_video_key_id.json":                 "video.key_id",
	"invalid_video_chunk_size.json":             "video.chunk_size",
	"invalid_video_size_bytes.json":             "video.size_bytes",
	"invalid_complete_plaintext_size_null.json": "video.plaintext_size_bytes",
	"invalid_video_container.json":              "video.container",
	"invalid_video_codec.json":                  "video.codec",
	"invalid_video_dimensions.json":             "video.width",
	"invalid_video_init_segment_bytes.json":     "video.init_segment_bytes",
	"invalid_complete_seek_index_null.json":     "video.seek_index",
	"invalid_seek_index_descending.json":        "video.seek_index",
	"invalid_recorder_time_source.json":         "recorder.time_source",
	"invalid_incomplete_end_reason.json":        "end_reason",
	"invalid_incomplete_codec_missing.json":     "video.codec",
	"invalid_incomplete_key_id_missing.json":    "video.key_id",
	"invalid_incomplete_triggers_null.json":     "triggers",
}

func TestValidateRejects(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixtureDir, "invalid_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(invalidCases) {
		t.Errorf("found %d invalid fixtures, table has %d", len(files), len(invalidCases))
	}
	for _, f := range files {
		if _, ok := invalidCases[filepath.Base(f)]; !ok {
			t.Errorf("fixture %s has no case in the table", filepath.Base(f))
		}
	}
	for name, field := range invalidCases {
		t.Run(name, func(t *testing.T) {
			sc, err := Decode(readFixture(t, name))
			if err == nil {
				err = sc.Validate()
			}
			if err == nil {
				t.Fatal("accepted an invalid sidecar")
			}
			var fe *FieldError
			if errors.As(err, &fe) {
				if fe.Field != field {
					t.Errorf("error field = %q, want %q (%v)", fe.Field, field, err)
				}
			} else if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name %q", err, field)
			}
		})
	}
}

func TestDecodeRejectsUnknownSchemaMajor(t *testing.T) {
	_, err := Decode(readFixture(t, "invalid_schema_major.json"))
	if !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("err = %v, want ErrUnknownSchema", err)
	}
	if _, err := Decode([]byte(`{"schema":"zrec.sidecar/02"}`)); !errors.Is(err, ErrUnknownSchema) {
		t.Errorf("zero-padded major: err = %v", err)
	}
	if _, err := Decode([]byte(`not json`)); err == nil {
		t.Error("accepted malformed JSON")
	}
	if _, err := Decode([]byte(`{"schema":"zrec.sidecar/2","clip_id":"not-a-ulid"}`)); err == nil {
		t.Error("accepted malformed clip_id")
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(readFixture(t, "valid_complete.json"), &doc); err != nil {
		t.Fatal(err)
	}
	doc["future_field"] = map[string]any{"x": 1}
	doc["camera"].(map[string]any)["zone"] = "porch"
	doc["video"].(map[string]any)["hdr"] = true
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestSidecarRoundTrip(t *testing.T) {
	sc, err := Decode(readFixture(t, "valid_incomplete.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"end_time":null`, `"duration_ms":null`, `"plaintext_size_bytes":null`,
		`"seek_index":null`, `"triggers":[]`, `"start_time":"2026-09-16T14:59:00.000Z"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshaled sidecar lacks %s: %s", want, b)
		}
	}
	again, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Validate(); err != nil {
		t.Fatal(err)
	}
}
