package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

func TestKeyFingerprint(t *testing.T) {
	var m zrec.MasterKey
	for i := range m {
		m[i] = byte(i)
	}
	c := &Config{Encryption: Encryption{MasterKeyText: m.String()}}
	got, err := KeyFingerprint(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := zrec.FormatKeyID(zrec.Derive(m).ID); got != want {
		t.Errorf("fingerprint %q, want %q", got, want)
	}

	// A bad key is reported as a key error and the error never carries the
	// key text.
	bad := m.String()[:len(m.String())-1] + "z"
	c.Encryption.MasterKeyText = bad
	_, err = KeyFingerprint(c)
	if !errors.Is(err, zrec.ErrKeyFormat) && !errors.Is(err, zrec.ErrKeyChecksum) {
		t.Fatalf("err %v, want a key error", err)
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("error leaks the key text: %v", err)
	}
}

// workedExample is the budget worked example of the plan (Task 4): 3
// cameras at 3200 kbps, 15 s pre-roll, 2 s GOP, 1 MiB chunks, 8 MiB parts,
// queues 256/64, 4 viewers and a 512 MiB backlog.
func workedExample(maxMemoryMB int) *Config {
	c := &Config{
		Relay:      Relay{MaxMemoryMB: maxMemoryMB, EgressBacklogMB: 512},
		Encryption: Encryption{ChunkSizeKiB: 1024},
		Storage:    Storage{Driver: "s3", S3: S3{PartSizeMiB: 8}},
		Live:       Live{MaxViewers: 4},
	}
	for i := 1; i <= 3; i++ {
		c.Cameras = append(c.Cameras, Camera{
			ID:                  fmt.Sprintf("cam-%d", i),
			PreRoll:             15 * time.Second,
			MaxBitrateKbps:      3200,
			MaxGOP:              2 * time.Second,
			RecorderQueueFrames: 256,
			ViewerQueueFrames:   64,
		})
	}
	return c
}

func TestBudgetWorkedExample(t *testing.T) {
	const (
		mib    = int64(1) << 20
		ring   = int64(8_500_000)  // 400000 × 17 × 1.25
		active = int64(11_285_760) // 2×1048576 + 800000 + 8×1048576
		queues = int64(13_653_333) // (256 + 64×4) × 400000 / 15
		camera = ring + active + queues
		live   = 4 * 2 * mib
		back   = 512 * mib
		rt     = 32 * mib
		total  = 3*camera + live + back + rt
	)

	r := Budget(workedExample(768))
	if len(r.Cameras) != 3 {
		t.Fatalf("len(Cameras) = %d, want 3", len(r.Cameras))
	}
	for i, cb := range r.Cameras {
		want := CameraBudget{ID: fmt.Sprintf("cam-%d", i+1), Ring: ring, Active: active, Queues: queues, Total: camera}
		if cb != want {
			t.Errorf("Cameras[%d] = %+v, want %+v", i, cb, want)
		}
	}
	if r.LiveBytes != live {
		t.Errorf("LiveBytes = %d, want %d", r.LiveBytes, live)
	}
	if r.BacklogBytes != back {
		t.Errorf("BacklogBytes = %d, want %d", r.BacklogBytes, back)
	}
	if r.RuntimeBytes != rt {
		t.Errorf("RuntimeBytes = %d, want %d", r.RuntimeBytes, rt)
	}
	if r.Total != total || total != 679_131_231 {
		t.Errorf("Total = %d, want %d (679131231)", r.Total, total)
	}
	if want := 768 * mib * 8 / 10; r.Limit != want {
		t.Errorf("Limit = %d, want %d", r.Limit, want)
	}
	if r.OK {
		t.Errorf("OK = true at max_memory_mb 768, want false (total %d > limit %d)", r.Total, r.Limit)
	}

	r = Budget(workedExample(1024))
	if r.Total != total {
		t.Errorf("Total at 1024 = %d, want %d", r.Total, total)
	}
	if !r.OK {
		t.Errorf("OK = false at max_memory_mb 1024, want true (total %d, limit %d)", r.Total, r.Limit)
	}
}

func TestBudgetLiveDisabledHasNoViewerTerms(t *testing.T) {
	c := workedExample(1024)
	c.Live.MaxViewers = 0
	r := Budget(c)
	if r.LiveBytes != 0 {
		t.Errorf("LiveBytes = %d, want 0", r.LiveBytes)
	}
	if want := int64(256 * 400000 / 15); r.Cameras[0].Queues != want {
		t.Errorf("Queues = %d, want %d (recorder queue only)", r.Cameras[0].Queues, want)
	}
}

func TestBudgetLiveConstantIsVariable(t *testing.T) {
	old := LivePerViewerBytes
	t.Cleanup(func() { LivePerViewerBytes = old })
	LivePerViewerBytes = 3 << 20
	if r := Budget(workedExample(1024)); r.LiveBytes != 4*(3<<20) {
		t.Errorf("LiveBytes = %d, want %d", r.LiveBytes, 4*(3<<20))
	}
}

func TestBudgetWriteTable(t *testing.T) {
	var buf bytes.Buffer
	Budget(workedExample(768)).WriteTable(&buf)
	out := buf.String()
	for _, want := range []string{"cam-1", "cam-2", "cam-3", "live", "backlog", "runtime", "TOTAL", "limit", "679131231", "OVER"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	Budget(workedExample(1024)).WriteTable(&buf)
	if out := buf.String(); strings.Contains(out, "OVER") || !strings.Contains(out, "OK") {
		t.Errorf("table at 1024 should say OK, not OVER:\n%s", out)
	}
}
