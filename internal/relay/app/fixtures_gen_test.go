//go:build fixtures

package app_test

// Regenerates the playback fixtures under testdata/clips/ (Task 36):
//
//	ZRELAY_FIXTURES_OUT=$PWD/testdata/clips \
//	  go test -tags fixtures -run TestGenerateClipFixtures -count=1 -timeout 5m ./internal/relay/app
//
// It runs the real relay pipeline (app.Run) against the in-process RTSP test
// server looping testdata/video/h264-640x360-gop2-10s.h264, holds a fake ONVIF
// motion trigger for about 33 s, and records to the s3 driver against a
// throwaway MinIO container (storagetest.StartMinIO; Docker required), so the
// committed video went through the S3 multipart commit path. The committed
// video and sidecar objects are then read back with S3 GETs and written out in
// the local driver's layout (<variant>/zrelay/clips/...), which zview-server
// serves. Two variants are derived from those bytes: tamper (one ciphertext bit
// flipped in a middle chunk) and incomplete (truncated mid-final-chunk, with the
// recovered-clip sidecar of Contract §7.4). The master key is the Contract §4.5
// vector key (00..1f).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/testutil"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
	"github.com/apikdech/zrelay-v2/pkg/storage/storagetest"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// fixtureRoot is storage.root of the generator's config and the first path
// element of the fixture trees.
const fixtureRoot = "zrelay"

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
// identity and 64 KiB chunks (so a 30 s clip has dozens of chunks), recording
// to a fresh MinIO bucket. The returned driver reads that bucket back.
func newFixtureRig(t *testing.T) (*rig, *s3.Driver) {
	t.Helper()
	rtsp := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: testutil.LoadAnnexB(t, h264Fixture), Loop: true})
	onvif := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	u, err := url.Parse(rtsp.URL())
	if err != nil {
		t.Fatal(err)
	}
	endpoint, accessKey, secretKey, bucket := storagetest.StartMinIO(t)
	store, err := s3.New(context.Background(), s3.Config{
		Endpoint: endpoint, Region: "us-east-1", Bucket: bucket, Root: fixtureRoot,
		AccessKey: accessKey, SecretKey: secretKey, UsePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
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
  driver: "s3"
  root: %q
  s3:
    endpoint: %q
    region: "us-east-1"
    bucket: %q
    use_path_style: true
    access_key: %q
    secret_key: %q
    part_size_mib: 5
defaults:
  pre_roll_seconds: 2
  post_roll_seconds: 1
  max_clip_seconds: 120
  trigger_timeout_seconds: 120
# live and restream default ON (design §5); these rigs run without them.
live: { max_viewers: 0 }
restream: { listen: "" }
cameras:
  - id: %q
    name: "Front Door"
    brand: "tapo"
    tags: ["exterior"]
    host: "127.0.0.1"
    credentials: { username: "admin", password: "secret" }
    rtsp: { port: %s, path: "/stream" }
    events: { source: "onvif", port: %d }
`, freePort(t), vectorKey().String(), fixtureRoot, endpoint, bucket, accessKey, secretKey, fixtureCamera, u.Port(), onvif.Port)
	p := filepath.Join(dir, "zrelay.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return &rig{rtsp: rtsp, onvif: onvif, cfg: cfg}, store
}

func TestGenerateClipFixtures(t *testing.T) {
	out := os.Getenv("ZRELAY_FIXTURES_OUT")
	if out == "" {
		t.Skip("set ZRELAY_FIXTURES_OUT to the testdata/clips directory")
	}
	mk := vectorKey()
	r, store := newFixtureRig(t)
	ctx := context.Background()

	run := start(t, r.cfg, app.Options{Version: "fixture"})
	eventually(t, "status/relay.json", 30*time.Second, func() bool {
		_, err := fixtureObject(ctx, store, "status/relay.json")
		return err == nil
	})
	// The video object is invisible until CompleteMultipartUpload, so there is no
	// "being written" milestone: hold the trigger for 33 s (the phone-profile e2e seeks to duration-1, which must lie beyond its 30 s ahead window), then wait for the sidecar.
	r.motion(true)
	time.Sleep(33 * time.Second)
	r.motion(false)
	eventually(t, "a clip sidecar", 40*time.Second, func() bool { return len(fixtureSidecars(t, store)) > 0 })
	if _, err := run.stop(t); err != nil {
		t.Fatalf("Run: %v", err)
	}

	scs := fixtureSidecars(t, store)
	if len(scs) != 1 {
		t.Fatalf("want exactly one clip, got %v", scs)
	}
	scKey := scs[0]
	sb, err := fixtureObject(ctx, store, scKey)
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
	video, err := fixtureObject(ctx, store, path.Join(path.Dir(scKey), sc.Video.Object))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(video)) != sc.Video.SizeBytes {
		t.Fatalf("video is %d bytes, sidecar says %d", len(video), sc.Video.SizeBytes)
	}
	if sc.SegmentIndex != 0 || sc.Video.SeekIndex == nil || sc.DurationMs == nil {
		t.Fatalf("unexpected sidecar: %+v", sc)
	}

	// The local driver's layout: <root>/<key's directory>.
	relDir := path.Join(fixtureRoot, path.Dir(scKey))

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

	// Replace the generated trees only; the README beside them stays.
	for _, d := range []string{"complete", "tamper", "incomplete"} {
		must(t, os.RemoveAll(filepath.Join(out, d)))
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

// fixtureSidecars lists the keys of the committed clip sidecars in the bucket.
func fixtureSidecars(t *testing.T, store storage.Reader) []string {
	t.Helper()
	var out []string
	for info, err := range store.List(context.Background(), "clips") {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, ".json") {
			out = append(out, info.Key)
		}
	}
	return out
}

// fixtureObject GETs the whole object at key.
func fixtureObject(ctx context.Context, r storage.Reader, key string) ([]byte, error) {
	or, err := r.OpenRange(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer or.Close()
	b, err := io.ReadAll(or)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != or.Total {
		return nil, errors.New("short read")
	}
	return b, nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
