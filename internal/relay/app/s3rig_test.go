//go:build integration || chaos

package app_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/testutil"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// fillerAUs returns aus with an H.264 filler-data NAL unit (type 12) of about
// perAU bytes appended to every access unit, which raises the bitrate of the
// short fixture to a camera-like one without changing what decodes.
func fillerAUs(aus []testutil.AU, perAU int) []testutil.AU {
	filler := make([]byte, perAU)
	for i := range filler {
		filler[i] = 0xff
	}
	filler[0] = 0x0c // nal_ref_idc 0, nal_unit_type 12
	filler[perAU-1] = 0x80
	out := make([]testutil.AU, len(aus))
	for i, au := range aus {
		n := make([][]byte, 0, len(au.NALUs)+1)
		n = append(n, au.NALUs...)
		out[i] = testutil.AU{NALUs: append(n, filler), IDR: au.IDR}
	}
	return out
}

// fakeCamera is one RTSP server and one ONVIF fake.
type fakeCamera struct {
	id    string
	rtsp  *testutil.RTSPServer
	onvif *testutil.FakeONVIF
}

func (c *fakeCamera) motion(active bool) {
	c.onvif.Queue(motionTopic, "Changed", events.Item{Name: "IsMotion", Value: fmt.Sprint(active)})
}

func startFakeCameras(t *testing.T, ids []string, bytesPerAU int) []*fakeCamera {
	t.Helper()
	aus := fillerAUs(testutil.LoadAnnexB(t, h264Fixture), bytesPerAU)
	var out []*fakeCamera
	for _, id := range ids {
		out = append(out, &fakeCamera{
			id:    id,
			rtsp:  testutil.StartRTSPServer(t, testutil.RTSPServerOpts{AUs: aus, Loop: true}),
			onvif: testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{}),
		})
	}
	return out
}

// s3Opts describe the config of an s3 rig.
type s3Opts struct {
	Endpoint, AccessKey, SecretKey, Bucket string
	MasterKey                              zrec.MasterKey
	HTTPListen                             string
	BacklogMB, MaxMemoryMB                 int
	GraceSeconds                           int
	MaxClipSeconds                         int
	// Cameras are the cameras to configure; Dead points them at ports where
	// nothing listens (a restart that must not record).
	Cameras []*fakeCamera
	Dead    bool
}

// writeS3Config writes a zrelay config for o under dir and returns its path.
func writeS3Config(t *testing.T, dir string, o s3Opts) string {
	t.Helper()
	var cams strings.Builder
	for _, c := range o.Cameras {
		rtspPort, onvifPort := portOf(t, c.rtsp.URL()), fmt.Sprint(c.onvif.Port)
		if o.Dead {
			rtspPort, onvifPort = portOf(t, "rtsp://"+freePort(t)), portOf(t, "http://"+freePort(t))
		}
		fmt.Fprintf(&cams, `  - id: %q
    name: %q
    brand: "tapo"
    host: "127.0.0.1"
    credentials: { username: "admin", password: "secret" }
    rtsp: { port: %s, path: "/stream" }
    events: { source: "onvif", port: %s }
`, c.id, c.id, rtspPort, onvifPort)
	}
	yml := fmt.Sprintf(`version: 2
allow_literal_secrets: true
relay:
  id: "test-relay"
  max_memory_mb: %d
  egress_backlog_mb: %d
  upload_workers: 2
  shutdown_grace_seconds: %d
  http_listen: %q
encryption:
  master_key: %q
storage:
  driver: "s3"
  root: "zrelay"
  s3:
    endpoint: %q
    region: "us-east-1"
    bucket: %q
    use_path_style: true
    access_key: %q
    secret_key: %q
    part_size_mib: 5
  breaker:
    failure_threshold: 3
    base_delay: "1s"
    max_delay: "10s"
defaults:
  pre_roll_seconds: 5
  post_roll_seconds: 2
  max_clip_seconds: %d
  max_gop_seconds: 2
  recorder_queue_frames: 64
  trigger_timeout_seconds: 120
cameras:
%s`, o.MaxMemoryMB, o.BacklogMB, o.GraceSeconds, o.HTTPListen, o.MasterKey.String(),
		o.Endpoint, o.Bucket, o.AccessKey, o.SecretKey, o.MaxClipSeconds, cams.String())
	p := filepath.Join(dir, "zrelay.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	_, rest, _ := strings.Cut(rawURL, "://")
	host, _, _ := strings.Cut(rest, "/")
	i := strings.LastIndex(host, ":")
	if i < 0 {
		t.Fatalf("no port in %q", rawURL)
	}
	return host[i+1:]
}

func newMasterKey(t *testing.T) zrec.MasterKey {
	t.Helper()
	mk, err := zrec.GenerateMasterKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

func loadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// newS3 opens the s3 driver the relay would use, for reading back results.
func newS3(t *testing.T, endpoint, ak, sk, bucket string) *s3.Driver {
	t.Helper()
	d, err := s3.New(context.Background(), s3.Config{
		Endpoint: endpoint, Region: "us-east-1", Bucket: bucket, Root: "zrelay",
		AccessKey: ak, SecretKey: sk, UsePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// listKeys returns the visible (non-dot) objects under prefix.
func listObjects(ctx context.Context, r storage.Reader, prefix string) ([]storage.ObjectInfo, error) {
	var out []storage.ObjectInfo
	for info, err := range r.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func readObject(ctx context.Context, r storage.Reader, key string) ([]byte, error) {
	or, err := r.OpenRange(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer or.Close()
	return io.ReadAll(or)
}
