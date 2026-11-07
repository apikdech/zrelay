// Command demo is a test-only helper, not shipped. It runs a fake Tapo camera
// (the RTSP test fixture on a loop, plus an ONVIF event service that reports
// motion and a person on a timer) and writes a ready-to-run zrelay and
// zview-server setup into a directory, so the whole stack can be tried on one
// machine without a camera or a NAS:
//
//	go run ./internal/testutil/cmd/demo [--dir .demo] [--every 60s] [--active 8s]
//
// It writes, under --dir:
//
//	.env                      CAMERA_*, RESTREAM_* (what deploy/.env holds in production)
//	secrets/zrelay_master     a throwaway master key, kept across runs
//	secrets/api_token_sha256  its API token hash
//	zrelay.yaml, zview.yaml   configs using the local storage driver under archive/
//
// then prints the commands that start zrelay and zview-server against them.
// The camera's ports change on every start, so zrelay.yaml is rewritten each
// time. It runs until interrupted.
// Exit codes: 0 clean stop, 1 runtime failure, 2 usage error.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/testutil"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const (
	defaultFixture = "testdata/video/h264-640x360-gop2-10s.h264"
	cameraID       = "demo-cam"
	cameraUser     = "admin"
	cameraPass     = "secret"
	restreamUser   = "demo"
	restreamPass   = "demo"

	motionTopic = "tns1:RuleEngine/CellMotionDetector/Motion"
	smartTopic  = "tns1:RuleEngine/TPSmartEventDetector/TPSmartEvent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil))
}

// info describes the running demo camera.
type info struct {
	Key        string
	RTSPPort   int
	ONVIFPort  int
	CameraUser string
	CameraPass string
}

// fatal is panicked by tb.Fatal* to unwind out of the testutil helpers.
type fatal struct{ msg string }

// tb is the slice of testing.TB the testutil fixtures use, outside a test.
type tb struct {
	testing.TB // nil: any method not defined below panics
	mu         sync.Mutex
	cleanups   []func()
	stderr     io.Writer
}

func (t *tb) Helper() {}
func (t *tb) Cleanup(f func()) {
	t.mu.Lock()
	t.cleanups = append(t.cleanups, f)
	t.mu.Unlock()
}
func (t *tb) Fatal(args ...any)                 { panic(fatal{fmt.Sprint(args...)}) }
func (t *tb) Fatalf(format string, args ...any) { panic(fatal{fmt.Sprintf(format, args...)}) }
func (t *tb) Errorf(format string, args ...any) {
	fmt.Fprintf(t.stderr, "demo: "+format+"\n", args...)
}
func (t *tb) runCleanups() {
	t.mu.Lock()
	fs := t.cleanups
	t.cleanups = nil
	t.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

// masterKey reuses the key in path, or generates one and writes it there, so
// clips recorded in earlier runs still decrypt.
func masterKey(path string) (zrec.MasterKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		m, err := zrec.ParseMasterKey(strings.TrimSpace(string(b)))
		if err != nil {
			return zrec.MasterKey{}, fmt.Errorf("%s does not hold a valid master key", path)
		}
		return m, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return zrec.MasterKey{}, err
	}
	m, err := zrec.GenerateMasterKey(rand.Reader)
	if err != nil {
		return zrec.MasterKey{}, err
	}
	return m, os.WriteFile(path, []byte(m.String()+"\n"), 0o600)
}

func writeFiles(dir, appDir string, m zrec.MasterKey, rtspPort, onvifPort int) error {
	secrets := filepath.Join(dir, "secrets")
	archive := filepath.Join(dir, "archive")
	sum := zrec.APITokenHash(zrec.Derive(m))
	files := []struct {
		name string
		body string
	}{
		{"secrets/api_token_sha256", hex.EncodeToString(sum[:]) + "\n"},
		{".env", fmt.Sprintf("CAMERA_IP=127.0.0.1\nCAMERA_USERNAME=%s\nCAMERA_PASSWORD=%s\nRESTREAM_USER=%s\nRESTREAM_PASS=%s\n",
			cameraUser, cameraPass, restreamUser, restreamPass)},
		{"zrelay.yaml", fmt.Sprintf(`# Written by the demo helper; rewritten on every start (the camera ports change).
version: 2
relay:
  id: "demo"
  max_memory_mb: 768
  egress_backlog_mb: 128
  upload_workers: 2
  shutdown_grace_seconds: 10
  http_listen: "127.0.0.1:9464"
encryption:
  master_key: "FILE:%[1]s/zrelay_master"
  chunk_size_kib: 1024
storage:
  driver: "local"               # production uses s3 (MinIO); see deploy/zrelay.example.yaml
  root: "zrelay"
  local:
    base_dir: %[2]q
live:
  listen: "127.0.0.1:9465"
  advertise_ip: "127.0.0.1"
  ice_udp_port: 8189
  max_viewers: 4
  api_token_sha256: ["FILE:%[1]s/api_token_sha256"]
restream:
  listen: "127.0.0.1:8554"
  username: "ENV:RESTREAM_USER"
  password: "ENV:RESTREAM_PASS"
defaults:
  pre_roll_seconds: 5
  post_roll_seconds: 5
  max_clip_seconds: 60
cameras:
  - id: %[3]q
    name: "Demo camera"
    brand: "tapo"
    host: "ENV:CAMERA_IP"
    credentials:
      username: "ENV:CAMERA_USERNAME"
      password: "ENV:CAMERA_PASSWORD"
    rtsp: { port: %[4]d, path: "/stream1" }
    events: { source: "onvif", port: %[5]d }
`, secrets, archive, cameraID, rtspPort, onvifPort)},
		{"zview.yaml", fmt.Sprintf(`# Written by the demo helper.
version: 2
server:
  listen: "127.0.0.1:8080"
  api_token_sha256: ["FILE:%[1]s/api_token_sha256"]
  app_dir: %[2]q
storage:
  driver: "local"
  root: "zrelay"
  local:
    base_dir: %[3]q
live:
  upstream: "http://127.0.0.1:9465"
`, secrets, appDir, archive)},
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// trigger reports motion and a person for active, every period.
func trigger(ctx context.Context, cam *testutil.FakeONVIF, every, active time.Duration, stderr io.Writer) {
	set := func(on bool) {
		v := strconv.FormatBool(on)
		cam.Queue(motionTopic, "Changed", events.Item{Name: "IsMotion", Value: v})
		cam.Queue(smartTopic, "Changed", events.Item{Name: "IsPeople", Value: v})
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		fmt.Fprintf(stderr, "demo: %s motion + person for %s\n", time.Now().UTC().Format(time.TimeOnly), active)
		set(true)
		select {
		case <-ctx.Done():
			return
		case <-time.After(active):
		}
		set(false)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(info)) (code int) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".demo", "`directory` for the generated configs, secrets and recorded clips")
	every := fs.Duration("every", 60*time.Second, "how often the fake camera reports motion and a person")
	active := fs.Duration("active", 8*time.Second, "how long each event stays active")
	fixture := fs.String("fixture", defaultFixture, "Annex-B H.264 `file` the fake camera replays")
	appDir := fs.String("app-dir", "web/dist", "built SPA `directory` zview-server serves")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *every <= 0 || *active <= 0 || *active >= *every {
		fmt.Fprintln(stderr, "usage: demo [--dir .demo] [--every 60s] [--active 8s] (active < every)")
		return 2
	}
	if _, err := os.Stat(*fixture); err != nil {
		fmt.Fprintf(stderr, "demo: %v (run from the repository root)\n", err)
		return 2
	}
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 2
	}
	absApp, err := filepath.Abs(*appDir)
	if err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 2
	}
	if err := os.MkdirAll(filepath.Join(absDir, "secrets"), 0o700); err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 1
	}

	t := &tb{stderr: stderr}
	defer t.runCleanups()
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(fatal)
			if !ok {
				panic(r)
			}
			fmt.Fprintf(stderr, "demo: %s\n", f.msg)
			code = 1
		}
	}()

	master, err := masterKey(filepath.Join(absDir, "secrets", "zrelay_master"))
	if err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 1
	}
	rtsp := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{
		AUs: testutil.LoadAnnexB(t, *fixture), Loop: true, Audio: true, RTCP: true,
	})
	u, err := url.Parse(rtsp.URL())
	if err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 1
	}
	rtspPort, _ := strconv.Atoi(u.Port())
	cam := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{Username: cameraUser, Password: cameraPass})
	if err := writeFiles(absDir, absApp, master, rtspPort, cam.Port); err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		trigger(ctx, cam, *every, *active, stderr)
	}()

	d := *dir
	fmt.Fprintf(stdout, `Demo camera %q is running: RTSP on 127.0.0.1:%d, ONVIF on 127.0.0.1:%d.
It reports motion and a person every %s for %s. Configs and secrets are in %s.

1. Start the relay (second terminal, repository root):
     go run ./cmd/zrelay --env-file %s/.env run --config %s/zrelay.yaml
2. Build the web app once, then start the viewer (third terminal):
     (cd web && pnpm install && pnpm build)
     go run ./cmd/zview-server --config %s/zview.yaml
3. Open http://127.0.0.1:8080 and import this throwaway demo master key:
     %s
   Clips appear on the Timeline after the first event; the Live tab shows the camera.
4. Optional: the RTSP restream plays in VLC at rtsp://%s:%s@127.0.0.1:8554/%s

Clips are written under %s/archive. Ctrl-C stops the camera. The key is kept
across runs; restart zrelay after restarting the demo (the camera ports change).
`, cameraID, rtspPort, cam.Port, *every, *active, d, d, d, d, master.String(), restreamUser, restreamPass, cameraID, d)

	if ready != nil {
		ready(info{Key: master.String(), RTSPPort: rtspPort, ONVIFPort: cam.Port, CameraUser: cameraUser, CameraPass: cameraPass})
	}
	<-ctx.Done()
	return 0
}
