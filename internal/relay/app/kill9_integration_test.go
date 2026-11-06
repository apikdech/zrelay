//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/storagetest"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// syncBuffer is a bytes.Buffer safe for a subprocess's output goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// buildZrelay compiles cmd/zrelay into dir.
func buildZrelay(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "zrelay")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/apikdech/zrelay-v2/cmd/zrelay")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build zrelay: %v\n%s", err, out)
	}
	return bin
}

// TestKill9ProducesIncomplete is Contract §7.4 end to end: a zrelay
// subprocess is killed with SIGKILL in the middle of a clip against MinIO, and
// the next start publishes an `incomplete` sidecar whose video opens with
// AllowIncomplete.
func TestKill9ProducesIncomplete(t *testing.T) {
	endpoint, ak, sk, bucket := storagetest.StartMinIO(t)
	// ~35 KB per access unit at 15 fps is about 4 Mbps: a 5 MiB part
	// completes within seconds.
	cams := startFakeCameras(t, []string{"cam-1"}, 35<<10)
	cam := cams[0]
	mk := newMasterKey(t)
	dir := t.TempDir()
	cfgPath := writeS3Config(t, dir, s3Opts{
		Endpoint: endpoint, AccessKey: ak, SecretKey: sk, Bucket: bucket, MasterKey: mk,
		HTTPListen: freePort(t), BacklogMB: 64, MaxMemoryMB: 768, GraceSeconds: 3,
		MaxClipSeconds: 600, Cameras: cams,
	})
	bin := buildZrelay(t, dir)

	// First life: a real process.
	var out syncBuffer
	cmd := exec.Command(bin, "run", "--config", cfgPath)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	drv := newS3(t, endpoint, ak, sk, bucket)
	ctx := context.Background()
	cam.motion(true)
	// Wait for a multipart upload with at least one completed part: that is
	// what a crash leaves recoverable.
	var inFlight storage.Incomplete
	eventually(t, "a multipart upload with a completed part", 120*time.Second, func() bool {
		incs, err := drv.ListIncomplete(ctx, "clips/cam-1", 0)
		if err != nil {
			return false
		}
		for _, inc := range incs {
			if inc.Size > 0 {
				inFlight = inc
				return true
			}
		}
		return false
	})
	if err := cmd.Process.Kill(); err != nil { // SIGKILL: no shutdown path runs
		t.Fatal(err)
	}
	killed = true
	_ = cmd.Wait()
	t.Logf("killed zrelay mid-clip: %s (%d bytes uploaded)", inFlight.Key, inFlight.Size)

	// The clip is invisible: no video, no sidecar.
	objs, err := listObjects(ctx, drv, "clips/cam-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".json") {
			t.Fatalf("a sidecar %s exists before recovery", o.Key)
		}
	}

	// Second life: recovery only recovers candidates older than two minutes
	// (Contract §7.4). A fake clock three minutes ahead makes the crashed
	// upload old enough without waiting; the cameras are unreachable so
	// nothing new is recorded.
	dead := writeS3Config(t, dir, s3Opts{
		Endpoint: endpoint, AccessKey: ak, SecretKey: sk, Bucket: bucket, MasterKey: mk,
		HTTPListen: freePort(t), BacklogMB: 64, MaxMemoryMB: 768, GraceSeconds: 3,
		MaxClipSeconds: 600, Cameras: cams, Dead: true,
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- app.Run(runCtx, loadConfig(t, dead), app.Options{
			Version: "test",
			Clock:   clock.NewFake(time.Now().Add(3 * time.Minute)),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	})

	var scKey string
	eventually(t, "the recovered sidecar", 60*time.Second, func() bool {
		objs, err := listObjects(ctx, drv, "clips/cam-1")
		if err != nil {
			return false
		}
		for _, o := range objs {
			if strings.HasSuffix(o.Key, ".json") {
				scKey = o.Key
				return true
			}
		}
		return false
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("second Run: %v", err)
	}
	done <- nil // for the cleanup

	raw, err := readObject(ctx, drv, scKey)
	if err != nil {
		t.Fatal(err)
	}
	var sc sidecar.Sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Status != sidecar.StatusIncomplete || sc.EndReason != "recovered" || sc.EndTime != nil || sc.DurationMs != nil {
		t.Fatalf("sidecar = status %q, end_reason %q, end_time %v, duration %v; want incomplete/recovered/null/null",
			sc.Status, sc.EndReason, sc.EndTime, sc.DurationMs)
	}
	if want := strings.TrimSuffix(scKey, ".json") + ".zrec"; !strings.HasSuffix(want, sc.Video.Object) {
		t.Errorf("sidecar video.object = %q, key %q", sc.Video.Object, want)
	}
	videoKey := strings.TrimSuffix(scKey, ".json") + ".zrec"
	fi, err := drv.Stat(ctx, videoKey)
	if err != nil {
		t.Fatalf("recovered video: %v", err)
	}
	if sc.Video.SizeBytes != fi.Size || fi.Size < inFlight.Size {
		t.Errorf("video size %d, sidecar says %d, uploaded before the kill %d", fi.Size, sc.Video.SizeBytes, inFlight.Size)
	}

	// The video opens with AllowIncomplete and yields plaintext until the
	// truncated tail.
	keys := zrec.Derive(mk)
	zr, err := zrec.OpenReader(storage.NewRangeReaderAt(ctx, drv, videoKey, fi.Size), fi.Size,
		zrec.Keyring{keys.ID: keys}, zrec.ReaderOptions{AllowIncomplete: true})
	if err != nil {
		t.Fatalf("open recovered video: %v", err)
	}
	var plain bytes.Buffer
	_, err = zr.WriteTo(&plain)
	if err != nil && !errors.Is(err, zrec.ErrTruncated) {
		t.Fatalf("decrypt recovered video: %v", err)
	}
	if plain.Len() == 0 {
		t.Error("the recovered video holds no plaintext")
	}
	if !bytes.Contains(plain.Bytes()[:min(plain.Len(), 4096)], []byte("ftyp")) {
		t.Error("the recovered plaintext does not start with an fMP4 init segment")
	}
	if strings.Contains(out.String(), "zk1-") || strings.Contains(out.String(), "secret") || strings.Contains(out.String(), sk) {
		t.Error("the subprocess log leaks a credential or key")
	}
}
