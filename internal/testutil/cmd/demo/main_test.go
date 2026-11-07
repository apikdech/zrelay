package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/zview"
	"github.com/apikdech/zrelay-v2/pkg/secretref"
)

const h264Fixture = "../../../../testdata/video/h264-640x360-gop2-10s.h264"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// start runs the demo in dir until the returned stop is called.
func start(t *testing.T, dir string, extra ...string) (info, *syncBuf, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan info, 1)
	out, errOut := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	args := append([]string{"--dir", dir, "--fixture", h264Fixture, "--app-dir", t.TempDir()}, extra...)
	go func() { done <- run(ctx, args, out, errOut, func(i info) { ready <- i }) }()
	select {
	case i := <-ready:
		return i, out, func() int {
			cancel()
			select {
			case c := <-done:
				return c
			case <-time.After(15 * time.Second):
				t.Fatal("run did not return after cancel")
				return -1
			}
		}
	case c := <-done:
		t.Fatalf("run exited early with %d: %s", c, errOut.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("demo not ready: %s", errOut.String())
	}
	return info{}, nil, nil
}

// loadEnv loads the demo's .env for the duration of the test.
func loadEnv(t *testing.T, path string) {
	t.Helper()
	for _, k := range []string{"CAMERA_IP", "CAMERA_USERNAME", "CAMERA_PASSWORD", "RESTREAM_USER", "RESTREAM_PASS"} {
		t.Setenv(k, "") // registers restore; LoadEnvFile only sets unset vars
		os.Unsetenv(k)
	}
	if err := secretref.LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
}

func TestDemoWritesConfigsBothServicesLoad(t *testing.T) {
	dir := t.TempDir()
	i, out, stop := start(t, dir)
	defer stop()

	for _, f := range []string{".env", "secrets/zrelay_master", "secrets/api_token_sha256"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, st.Mode().Perm())
		}
	}
	loadEnv(t, filepath.Join(dir, ".env"))
	cfg, err := config.Load(filepath.Join(dir, "zrelay.yaml"))
	if err != nil {
		t.Fatalf("zrelay.yaml: %v", err)
	}
	if cfg.Storage.Driver != "local" || len(cfg.Cameras) != 1 {
		t.Fatalf("driver %q, %d cameras", cfg.Storage.Driver, len(cfg.Cameras))
	}
	cam := cfg.Cameras[0]
	if cam.EventsPort != i.ONVIFPort || cam.Host != "127.0.0.1" {
		t.Errorf("camera events %s:%d, want 127.0.0.1:%d", cam.Host, cam.EventsPort, i.ONVIFPort)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(dir, "zrelay.yaml"))), "zk1-") {
		t.Error("zrelay.yaml holds the master key literally; it must be a FILE: ref")
	}
	if _, err := zview.LoadConfig(filepath.Join(dir, "zview.yaml")); err != nil {
		t.Fatalf("zview.yaml: %v", err)
	}
	if !strings.Contains(out.String(), "zrelay --env-file") || !strings.Contains(out.String(), i.Key) {
		t.Errorf("instructions missing the run command or the key:\n%s", out.String())
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDemoReusesMasterKeyAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	first, _, stop := start(t, dir)
	if c := stop(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	second, _, stop2 := start(t, dir)
	defer stop2()
	if first.Key != second.Key {
		t.Error("a restart generated a new master key; earlier clips would no longer decrypt")
	}
}

func TestDemoCameraEmitsTapoMotionAndPerson(t *testing.T) {
	dir := t.TempDir()
	i, _, stop := start(t, dir, "--every", "300ms", "--active", "200ms")
	defer stop()

	pp := &events.PullPoint{
		Camera: "demo-cam", Host: "127.0.0.1", Port: i.ONVIFPort,
		Username: i.CameraUser, Password: i.CameraPass,
		Rules: events.TapoRules(), Clock: clock.Real,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seen := make(chan events.Kind, 16)
	go func() {
		_ = pp.Run(ctx, func(s events.Signal) {
			if s.Active {
				seen <- s.Kind
			}
		}, func(bool) {})
	}()
	want := map[events.Kind]bool{events.KindMotion: false, events.KindPerson: false}
	for !want[events.KindMotion] || !want[events.KindPerson] {
		select {
		case k := <-seen:
			want[k] = true
		case <-ctx.Done():
			t.Fatalf("signals seen: %v", want)
		}
	}
}

type countSink struct {
	mu  sync.Mutex
	aus int
}

func (s *countSink) Codec(camera.Codec, camera.TimeSource) {}
func (s *countSink) Video(*camera.AccessUnit) {
	s.mu.Lock()
	s.aus++
	s.mu.Unlock()
}
func (s *countSink) Audio(*camera.AudioPacket) {}

func TestDemoCameraServesRTSP(t *testing.T) {
	i, _, stop := start(t, t.TempDir())
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sink := &countSink{}
	_ = camera.RunRTSP(ctx, fmt.Sprintf("rtsp://%s:%s@127.0.0.1:%d/stream1", i.CameraUser, i.CameraPass, i.RTSPPort), sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.aus < 15 {
		t.Fatalf("got %d access units in 3 s", sink.aus)
	}
}
