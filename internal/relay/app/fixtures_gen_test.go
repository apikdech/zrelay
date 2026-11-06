//go:build fixtures

package app_test

// Regenerates the playback fixtures under testdata/clips/ (Task 36):
//
//	ZRELAY_FIXTURES_OUT=$PWD/testdata/clips \
//	  go test -tags fixtures -run TestGenerateClipFixtures -count=1 -timeout 5m ./internal/relay/app
//
// It runs the real relay pipeline (app.Run) against the in-process RTSP test
// server looping testdata/video/h264-640x360-gop2-10s.h264, holds a fake ONVIF
// motion trigger for about 30 s, and records to the local driver under
// t.TempDir(). The committed objects are then copied out, together with two
// derived variants: tamper (one ciphertext bit flipped in a middle chunk) and
// incomplete (truncated mid-final-chunk, with the recovered-clip sidecar of
// Contract §7.4). The master key is the Contract §4.5 vector key (00..1f).

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/testutil"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const fixtureCamera = "front-door"

type fixtureClip struct {
	ClipID string `json:"clip_id"`
	Dir    string `json:"dir"`
}

type fixtureManifest struct {
	MasterKey      string      `json:"master_key"`
	MasterKeyHex   string      `json:"master_key_hex"`
	KeyIDHex       string      `json:"key_id_hex"`
	Camera         string      `json:"camera"`
	ChunkSize      int         `json:"chunk_size"`
	Codec          string      `json:"codec"`
	Width          int         `json:"width"`
	Height         int         `json:"height"`
	DurationMs     int64       `json:"duration_ms"`
	Complete       fixtureClip `json:"complete"`
	Tamper         fixtureClip `json:"tamper"`
	Incomplete     fixtureClip `json:"incomplete"`
	NumChunks      int         `json:"num_chunks"`
	TamperChunk    int         `json:"tamper_chunk"`
	TamperFailAtMs int         `json:"tamper_fail_at_ms"`
}

func vectorKey() zrec.MasterKey {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	return zrec.MasterKey(raw)
}

// newFixtureRig is newRigFor with the vector master key, the fixture camera
// identity and 64 KiB chunks (so a 30 s clip has dozens of chunks).
func newFixtureRig(t *testing.T) *rig {
	t.Helper()
	rtsp := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: testutil.LoadAnnexB(t, h264Fixture), Loop: true})
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	u, err := url.Parse(rtsp.URL())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "archive")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	yml := fmt.Sprintf(`version: 2
allow_literal_secrets: true
relay:
  id: "fixture-relay"
  max_memory_mb: 768
  egress_backlog_mb: 64
  shutdown_grace_seconds: 3
  http_listen: %q
encryption:
  master_key: %q
  chunk_size_kib: 64
storage:
  driver: "local"
  root: "zrelay"
  local:
    base_dir: %q
defaults:
  pre_roll_seconds: 2
  post_roll_seconds: 1
  max_clip_seconds: 120
  trigger_timeout_seconds: 120
cameras:
  - id: %q
    name: "Front Door"
    brand: "tapo"
    tags: ["exterior"]
    host: "127.0.0.1"
    credentials: { username: "admin", password: "secret" }
    rtsp: { port: %s, path: "/stream" }
    events: { source: "onvif", port: %d }
`, freePort(t), vectorKey().String(), base, fixtureCamera, u.Port(), onvif.Port)
	p := filepath.Join(dir, "zrelay.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return &rig{rtsp: rtsp, onvif: onvif, base: base, cfg: cfg}
}

func TestGenerateClipFixtures(t *testing.T) {
	out := os.Getenv("ZRELAY_FIXTURES_OUT")
	if out == "" {
		t.Skip("set ZRELAY_FIXTURES_OUT to the testdata/clips directory")
	}
	mk := vectorKey()
	r := newFixtureRig(t)

	run := start(t, r.cfg, app.Options{Version: "fixture"})
	statusFile := filepath.Join(r.base, "zrelay", "status", "relay.json")
	eventually(t, "status/relay.json", 30*time.Second, func() bool {
		_, err := os.Stat(statusFile)
		return err == nil
	})
	r.motion(true)
	eventually(t, "a video object being written", 40*time.Second, func() bool {
		return len(files(r.base, func(rel string) bool { return strings.Contains(rel, "/clips/") && strings.Contains(rel, ".zrec") })) > 0
	})
	time.Sleep(30 * time.Second)
	r.motion(false)
	eventually(t, "a clip sidecar", 40*time.Second, func() bool { return len(clipSidecars(r.base)) > 0 })
	if _, err := run.stop(t); err != nil {
		t.Fatalf("Run: %v", err)
	}

	scs := clipSidecars(r.base)
	if len(scs) != 1 {
		t.Fatalf("want exactly one clip, got %v", scs)
	}
	scPath := scs[0]
	sb, err := os.ReadFile(scPath)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sidecar.Decode(sb)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	video, err := os.ReadFile(filepath.Join(filepath.Dir(scPath), sc.Video.Object))
	if err != nil {
		t.Fatal(err)
	}
	if sc.SegmentIndex != 0 || sc.Video.SeekIndex == nil || sc.DurationMs == nil {
		t.Fatalf("unexpected sidecar: %+v", sc)
	}

	rel, err := filepath.Rel(r.base, scPath)
	if err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Dir(rel)

	chunk := int(sc.Video.ChunkSize)
	hdrLen := int(video[6])<<8 | int(video[7])
	n := (len(video) - hdrLen + chunk + 15) / (chunk + 16)
	if n < 8 {
		t.Fatalf("only %d chunks; the fixtures need a clip long enough for a middle chunk", n)
	}
	tamperChunk := n / 2
	failAt := 0
	for _, e := range *sc.Video.SeekIndex {
		if e[1] <= int64(tamperChunk*chunk) {
			failAt = int(e[0])
		}
	}

	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	write := func(variant string, sidecarJSON, videoBytes []byte) {
		dir := filepath.Join(out, variant, relDir)
		must(t, os.MkdirAll(dir, 0o755))
		must(t, os.WriteFile(filepath.Join(dir, sc.Video.Object), videoBytes, 0o644))
		must(t, os.WriteFile(filepath.Join(dir, strings.TrimSuffix(sc.Video.Object, ".zrec")+".json"), sidecarJSON, 0o644))
	}
	write("complete", sb, video)

	tampered := append([]byte(nil), video...)
	tampered[hdrLen+tamperChunk*(chunk+16)+100] ^= 0x01
	write("tamper", sb, tampered)

	// Incomplete: cut the object mid-way through its final chunk and describe
	// it as Contract §7.4 recovery does.
	lastLen := len(video) - hdrLen - (n-1)*(chunk+16)
	truncated := append([]byte(nil), video[:len(video)-lastLen/2-1]...)
	inc := *sc
	inc.Status = sidecar.StatusIncomplete
	inc.EndReason = "recovered"
	inc.EndTime, inc.DurationMs = nil, nil
	inc.Triggers = []sidecar.Trigger{}
	inc.Video.PlaintextSizeBytes, inc.Video.SeekIndex = nil, nil
	inc.Video.SizeBytes = int64(len(truncated))
	if err := inc.Validate(); err != nil {
		t.Fatal(err)
	}
	ib, err := json.MarshalIndent(&inc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write("incomplete", ib, truncated)

	m := fixtureManifest{
		MasterKey: mk.String(), MasterKeyHex: hex.EncodeToString(mk[:]), KeyIDHex: sc.Video.KeyID,
		Camera: fixtureCamera, ChunkSize: chunk, Codec: sc.Video.Codec, Width: sc.Video.Width, Height: sc.Video.Height,
		DurationMs: *sc.DurationMs,
		Complete:   fixtureClip{ClipID: sc.ClipID.String(), Dir: "complete"},
		Tamper:     fixtureClip{ClipID: sc.ClipID.String(), Dir: "tamper"},
		Incomplete: fixtureClip{ClipID: sc.ClipID.String(), Dir: "incomplete"},
		NumChunks:  n, TamperChunk: tamperChunk, TamperFailAtMs: failAt,
	}
	mb, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(out, "manifest.json"), append(mb, '\n'), 0o644))
	t.Logf("wrote fixtures under %s: %d chunks, tamper chunk %d (~%d ms), duration %d ms, video %d bytes", out, n, tamperChunk, failAt, *sc.DurationMs, len(video))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
