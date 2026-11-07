package app_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage/local"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// seedCrashedClip leaves what a process killed mid-clip leaves in storage: an
// unfinished video upload whose descriptor is written, and the staging
// sidecar of Contract §7.3 step 0. Both carry the current time as their
// modification time, as they would after a restart a few seconds later.
func seedCrashedClip(t *testing.T, base, cam string, mk zrec.MasterKey) {
	t.Helper()
	d, err := local.New(base, "zrelay")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Millisecond)
	var entropy [10]byte
	id, err := ulid.New(uint64(start.UnixMilli()), bytes.NewReader(entropy[:]))
	if err != nil {
		t.Fatal(err)
	}
	keys := sidecar.ClipKeys(cam, start, id)

	var buf bytes.Buffer
	w, err := zrec.NewWriter(&buf, zrec.WriterParams{
		Keys: zrec.Derive(mk), ClipID: id, StartUnixMs: start.UnixMilli(), CameraID: cam,
	})
	if err != nil {
		t.Fatal(err)
	}
	hdr := bytes.Clone(buf.Bytes()[:w.HeaderLen()])
	w.Abort()

	sc := sidecar.Sidecar{
		Schema: sidecar.Schema, ClipID: id, SeriesID: id,
		Camera:    sidecar.Camera{ID: cam, Name: "Camera One", Brand: "tapo", Tags: []string{}},
		StartTime: sidecar.Time{Time: start},
		PreRollMs: 2000,
		Video: sidecar.Video{
			Object: keys.Video[len(keys.Dir)+1:], FormatVersion: 1, KeyID: "a1b2c3d4e5f60718",
			ChunkSize: 1 << 20, Container: "fmp4", Codec: "avc1.640028", Width: 640, Height: 360,
			InitSegmentBytes: 812,
		},
		Recorder: sidecar.Recorder{RelayID: "test-relay", Version: "test", TimeSource: "arrival"},
	}
	doc, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutSmall(t.Context(), keys.StagingSidecar, doc, "application/json"); err != nil {
		t.Fatal(err)
	}
	ow, err := d.Create(t.Context(), keys.Video)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ow.Write(append(hdr, bytes.Repeat([]byte{0xa5}, 4096)...)); err != nil {
		t.Fatal(err)
	}
	// Neither Commit nor Abort: the process was killed.
}

// TestRecoveryRescansClipsTooYoungAtStartup: a clip interrupted by a crash
// and a restart within the 2 minute age filter is "too young" on the first
// scan. The first scan must not count as the recovery, because nothing would
// revisit the clip (Contract §7.4: kill -9 mid-clip produces an incomplete
// sidecar after restart). The relay scans again once the clip has aged,
// still without touching anything written after its own start.
func TestRecoveryRescansClipsTooYoungAtStartup(t *testing.T) {
	logs := captureLogs(t)
	t.Cleanup(app.SetRecoveryRetryEvery(100 * time.Millisecond))
	r := newRig(t, "")
	mk, err := zrec.ParseMasterKey(r.cfg.Encryption.MasterKeyText)
	if err != nil {
		t.Fatal(err)
	}
	seedCrashedClip(t, r.base, "cam-1", mk)
	st := newDown(t, r.base)
	st.down.Store(false)
	clk := &shiftClock{Clock: clock.Real}
	o := st.opts(t)
	o.Clock = clk
	start(t, r.cfg, o)

	// Right after the restart the crashed clip is younger than 2 minutes.
	eventually(t, "the first recovery scan", 20*time.Second, func() bool {
		_, list, ok := st.counts()
		return list >= 1 && ok >= 1
	})
	if got := clipSidecars(r.base); len(got) != 0 {
		t.Fatalf("the clip was recovered while younger than the age filter: %v", got)
	}

	// Time passes; the crashed clip is old enough for recovery.
	clk.advance(3 * time.Minute)
	eventually(t, "the crashed clip to be recovered", 20*time.Second, func() bool {
		return len(clipSidecars(r.base)) == 1
	})
	b, err := os.ReadFile(clipSidecars(r.base)[0])
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sidecar.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Status != sidecar.StatusIncomplete || sc.EndReason != "recovered" {
		t.Errorf("sidecar status=%q end_reason=%q, want an incomplete, recovered clip", sc.Status, sc.EndReason)
	}
	eventually(t, "recovery to report done", 10*time.Second, func() bool {
		return strings.Contains(logs.text(), "startup recovery done recovered=1")
	})
}
