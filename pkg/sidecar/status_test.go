package sidecar

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const contractStatus = `{
  "schema": "zrec.status/1",
  "relay_id": "home-mini",
  "version": "2.0.0",
  "updated_at": "2026-09-16T15:00:00Z",
  "key_id": "a1b2c3d4e5f60718",
  "cameras": [
    { "id": "front-door", "name": "Front Door", "rtsp": "up", "events": "up",
      "since": "2026-09-16T09:12:04Z", "last_clip_at": "2026-09-16T14:59:00Z" }
  ]
}`

func TestStatusAdditiveStorageFieldRoundTrips(t *testing.T) {
	// A Contract §7.5 status without storage decodes, and storage stays nil.
	var plain Status
	if err := json.Unmarshal([]byte(contractStatus), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.Storage != nil {
		t.Errorf("storage = %+v, want nil", plain.Storage)
	}
	if plain.Schema != StatusSchema || len(plain.Cameras) != 1 || plain.Cameras[0].RTSP != "up" ||
		plain.Cameras[0].LastClipAt == nil {
		t.Errorf("decoded %+v", plain)
	}
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"storage"`) {
		t.Errorf("nil storage marshaled: %s", b)
	}

	// With the additive storage field it round-trips exactly.
	st := plain
	st.Storage = &StorageStatus{
		State:        "closed",
		Since:        Time{time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)},
		BacklogBytes: 4096,
	}
	st.Cameras = []CameraStatus{{ID: "front-door", Name: "Front Door", RTSP: "degraded", Events: "down",
		Since: Time{time.Date(2026, 9, 16, 9, 12, 4, 0, time.UTC)}}}
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"storage":{"state":"closed","since":"2026-11-06T09:00:00.000Z","backlog_bytes":4096}`,
		`"last_clip_at":null`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshaled status lacks %s: %s", want, b)
		}
	}
	var back Status
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, st) {
		t.Errorf("round trip\n got %+v\nwant %+v", back, st)
	}
}
