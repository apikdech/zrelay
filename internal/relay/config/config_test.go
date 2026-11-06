package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/secretref"
)

const exampleConfig = "../../../deploy/zrelay.example.yaml"

// testTokenHash is a well-formed SHA-256 hex digest (of the empty string).
const testTokenHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// writeFile writes content under dir with mode 0600 and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// loadExample loads deploy/zrelay.example.yaml with its FILE: secrets
// redirected from /run/secrets/ to temp files and its ENV: refs set.
func loadExample(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "zrelay_master", "zk1-example-master-key\n")
	writeFile(t, dir, "s3_secret_key", "s3-secret\n")
	writeFile(t, dir, "api_token_sha256", testTokenHash+"\n")
	for k, v := range map[string]string{
		"S3_ENDPOINT":     "https://nas.example.ts.net:9000",
		"S3_ACCESS_KEY":   "s3-access",
		"TAILSCALE_IP":    "100.64.0.7",
		"RESTREAM_USER":   "viewer",
		"RESTREAM_PASS":   "restream-pass",
		"CAMERA_IP":       "192.0.2.20",
		"CAMERA_USERNAME": "admin",
		"CAMERA_PASSWORD": "cam-pass",
	} {
		t.Setenv(k, v)
	}
	src, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	yml := strings.ReplaceAll(string(src), "FILE:/run/secrets/", "FILE:"+dir+"/")
	cfg, err := Load(writeFile(t, dir, "zrelay.yaml", yml))
	if err != nil {
		t.Fatalf("Load(example): %v", err)
	}
	return cfg, dir
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, _ := loadExample(t)

	if cfg.Version != 2 {
		t.Errorf("Version = %d, want 2", cfg.Version)
	}
	if cfg.Storage.Driver != "s3" {
		t.Errorf("Storage.Driver = %q, want s3", cfg.Storage.Driver)
	}
	if cfg.Storage.S3.PartSizeMiB != 8 {
		t.Errorf("Storage.S3.PartSizeMiB = %d, want 8", cfg.Storage.S3.PartSizeMiB)
	}
	if cfg.Live.MaxViewers != 4 {
		t.Errorf("Live.MaxViewers = %d, want 4", cfg.Live.MaxViewers)
	}
	if len(cfg.Cameras) != 1 || cfg.Cameras[0].ID != "tapo-c220" {
		t.Fatalf("Cameras = %+v, want one camera tapo-c220", cfg.Cameras)
	}
	cam := cfg.Cameras[0]
	if cam.EventsPort != 2020 {
		t.Errorf("EventsPort = %d, want 2020", cam.EventsPort)
	}
	if cam.PreRoll != 15*time.Second {
		t.Errorf("PreRoll = %v, want 15s", cam.PreRoll)
	}
}

func TestLoadExampleConfigResolvesEveryField(t *testing.T) {
	cfg, dir := loadExample(t)

	checks := []struct {
		name      string
		got, want any
	}{
		{"Relay.ID", cfg.Relay.ID, "home-mini"},
		{"Relay.MaxMemoryMB", cfg.Relay.MaxMemoryMB, 768},
		{"Relay.EgressBacklogMB", cfg.Relay.EgressBacklogMB, 512},
		{"Relay.UploadWorkers", cfg.Relay.UploadWorkers, 2},
		{"Relay.ShutdownGraceSeconds", cfg.Relay.ShutdownGraceSeconds, 30},
		{"Relay.HTTPListen", cfg.Relay.HTTPListen, "127.0.0.1:9464"},
		{"Encryption.MasterKey (ref kept)", cfg.Encryption.MasterKey, "FILE:" + dir + "/zrelay_master"},
		{"Encryption.MasterKeyText", cfg.Encryption.MasterKeyText, "zk1-example-master-key"},
		{"Encryption.ChunkSizeKiB", cfg.Encryption.ChunkSizeKiB, 1024},
		{"Storage.Root", cfg.Storage.Root, "zrelay"},
		{"Storage.S3.Endpoint", cfg.Storage.S3.Endpoint, "https://nas.example.ts.net:9000"},
		{"Storage.S3.Region", cfg.Storage.S3.Region, "us-east-1"},
		{"Storage.S3.Bucket", cfg.Storage.S3.Bucket, "cctv"},
		{"Storage.S3.UsePathStyle", cfg.Storage.S3.UsePathStyle, true},
		{"Storage.S3.AccessKey", cfg.Storage.S3.AccessKey, "s3-access"},
		{"Storage.S3.SecretKey", cfg.Storage.S3.SecretKey, "s3-secret"},
		{"Storage.Local.BaseDir", cfg.Storage.Local.BaseDir, "/tmp/zrelay-test"},
		{"Storage.Breaker.FailureThreshold", cfg.Storage.Breaker.FailureThreshold, 5},
		{"Storage.Breaker.BaseDelay", cfg.Storage.Breaker.BaseDelay, time.Second},
		{"Storage.Breaker.MaxDelay", cfg.Storage.Breaker.MaxDelay, 5 * time.Minute},
		{"Live.Listen", cfg.Live.Listen, "0.0.0.0:9465"},
		{"Live.AdvertiseIP", cfg.Live.AdvertiseIP, "100.64.0.7"},
		{"Live.ICEUDPPort", cfg.Live.ICEUDPPort, 8189},
		{"Live.APITokenSHA256", strings.Join(cfg.Live.APITokenSHA256, ","), testTokenHash},
		{"Restream.Listen", cfg.Restream.Listen, "0.0.0.0:8554"},
		{"Restream.Username", cfg.Restream.Username, "viewer"},
		{"Restream.Password", cfg.Restream.Password, "restream-pass"},
		{"Defaults.PreRollSeconds", cfg.Defaults.PreRollSeconds, 15},
		{"Defaults.ViewerQueueFrames", cfg.Defaults.ViewerQueueFrames, 64},
		{"AllowLiteralSecrets", cfg.AllowLiteralSecrets, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	cam := cfg.Cameras[0]
	camChecks := []struct {
		name      string
		got, want any
	}{
		{"Name", cam.Name, "Tapo C220"},
		{"Brand", cam.Brand, "tapo"},
		{"Tags", strings.Join(cam.Tags, ","), "interior"},
		{"Host", cam.Host, "192.0.2.20"},
		{"Username", cam.Username, "admin"},
		{"Password", cam.Password, "cam-pass"},
		{"RTSP.Port", cam.RTSP.Port, 554},
		{"RTSP.Path", cam.RTSP.Path, "/stream1"},
		{"Events.Source", cam.Events.Source, "onvif"},
		{"PostRoll", cam.PostRoll, 10 * time.Second},
		{"MaxClip", cam.MaxClip, 300 * time.Second},
		{"MaxBitrateKbps", cam.MaxBitrateKbps, 4000},
		{"MaxGOP", cam.MaxGOP, 4 * time.Second},
		{"TriggerTimeout", cam.TriggerTimeout, 60 * time.Second},
		{"RecorderQueueFrames", cam.RecorderQueueFrames, 256},
		{"ViewerQueueFrames", cam.ViewerQueueFrames, 64},
		{"len(Rules)", len(cam.Rules), 0},
	}
	for _, c := range camChecks {
		if c.got != c.want {
			t.Errorf("camera %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestValidBasesLoad(t *testing.T) {
	for _, name := range []string{"valid_local.yaml", "valid_s3.yaml"} {
		if _, err := Load(filepath.Join("testdata", name)); err != nil {
			t.Errorf("Load(%s): %v", name, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		file string
		path string // field path the error must mention
	}{
		{"duplicate_camera_id", "cameras[1].id"},
		{"camera_id_uppercase", "cameras[0].id"},
		{"driver_smb", "storage.driver"},
		{"smb_block", "storage.smb"},
		{"sftp_block", "storage.sftp"},
		{"unknown_retention", "retention"},
		{"unknown_relay_mlock", "relay.mlock"},
		{"keychain_ref", "encryption.master_key"},
		{"part_size_4", "storage.s3.part_size_mib"},
		{"endpoint_not_url", "storage.s3.endpoint"},
		{"breaker_base_over_max", "storage.breaker.base_delay"},
		{"live_max_viewers_negative", "live.max_viewers"},
		{"chunk_96", "encryption.chunk_size_kib"},
		{"chunk_32", "encryption.chunk_size_kib"},
		{"chunk_32768", "encryption.chunk_size_kib"},
		{"rule_empty_topic", "cameras[0].events.rules[0].topic"},
		{"rule_match_all_without_item", "cameras[0].events.rules[0].item"},
		{"version_1", "version"},
		{"local_root_repeat", "storage.local.base_dir"},
		{"non_tapo_without_rules", "cameras[0].events.rules"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", "reject", tc.file+".yaml"))
			if err == nil {
				t.Fatal("Load: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("error %q does not mention %q", err, tc.path)
			}
		})
	}
}

func TestKeychainRefIsUnsupported(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "reject", "keychain_ref.yaml"))
	if !errors.Is(err, secretref.ErrUnsupported) {
		t.Fatalf("err = %v, want secretref.ErrUnsupported", err)
	}
}

// minimalCamera is a config fragment around one camera block.
func minimalConfig(camera string) string {
	return `version: 2
relay: { id: "test", max_memory_mb: 768, egress_backlog_mb: 64 }
encryption: { master_key: "ENV:ZR_TEST_MASTER" }
storage: { driver: "local", root: "zrelay", local: { base_dir: "/x/archive" } }
cameras:
` + camera
}

func TestHostAndPortAcceptEnvRefs(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("CAMERA_IP", "192.0.2.33")
	t.Setenv("ONVIF_PORT", "8000")
	t.Setenv("CAM_USER", "admin")
	t.Setenv("CAM_PASS", "pw")
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", minimalConfig(`  - id: "front-door"
    brand: "tapo"
    host: "ENV:CAMERA_IP"
    credentials: { username: "ENV:CAM_USER", password: "ENV:CAM_PASS" }
    rtsp: { port: 554, path: "/stream1" }
    events: { source: "onvif", port: "ENV:ONVIF_PORT" }
`))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cam := cfg.Cameras[0]
	if cam.Host != "192.0.2.33" {
		t.Errorf("Host = %q, want 192.0.2.33", cam.Host)
	}
	if cam.EventsPort != 8000 {
		t.Errorf("EventsPort = %d, want 8000", cam.EventsPort)
	}
	if cam.Username != "admin" || cam.Password != "pw" {
		t.Errorf("credentials = %q/%q, want admin/pw", cam.Username, cam.Password)
	}
}

func TestEventsPortFromEnvMustBeNumeric(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("ONVIF_PORT", "not-a-port")
	t.Setenv("CAM_USER", "admin")
	t.Setenv("CAM_PASS", "pw")
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfig(`  - id: "front-door"
    brand: "tapo"
    host: "192.0.2.33"
    credentials: { username: "ENV:CAM_USER", password: "ENV:CAM_PASS" }
    rtsp: { port: 554, path: "/stream1" }
    events: { port: "ENV:ONVIF_PORT" }
`))
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "cameras[0].events.port") {
		t.Fatalf("err = %v, want error mentioning cameras[0].events.port", err)
	}
}

func TestDefaultsApplied(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("CAM_USER", "admin")
	t.Setenv("CAM_PASS", "pw")
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfig(`  - id: "front-door"
    brand: "tapo"
    host: "192.0.2.33"
    credentials: { username: "ENV:CAM_USER", password: "ENV:CAM_PASS" }
    rtsp: { path: "/stream1" }
`))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cam := cfg.Cameras[0]
	checks := []struct {
		name      string
		got, want any
	}{
		{"Relay.HTTPListen", cfg.Relay.HTTPListen, "127.0.0.1:9464"},
		{"Relay.UploadWorkers", cfg.Relay.UploadWorkers, 2},
		{"Relay.ShutdownGraceSeconds", cfg.Relay.ShutdownGraceSeconds, 30},
		{"Encryption.ChunkSizeKiB", cfg.Encryption.ChunkSizeKiB, 1024},
		{"Storage.S3.PartSizeMiB", cfg.Storage.S3.PartSizeMiB, 8},
		{"Storage.Breaker.FailureThreshold", cfg.Storage.Breaker.FailureThreshold, 5},
		{"Storage.Breaker.BaseDelay", cfg.Storage.Breaker.BaseDelay, time.Second},
		{"Storage.Breaker.MaxDelay", cfg.Storage.Breaker.MaxDelay, 5 * time.Minute},
		{"Live.MaxViewers", cfg.Live.MaxViewers, 0},
		{"cam.RTSP.Port", cam.RTSP.Port, 554},
		{"cam.Events.Source", cam.Events.Source, "onvif"},
		{"cam.EventsPort", cam.EventsPort, 2020},
		{"cam.PreRoll", cam.PreRoll, 15 * time.Second},
		{"cam.PostRoll", cam.PostRoll, 10 * time.Second},
		{"cam.MaxClip", cam.MaxClip, 300 * time.Second},
		{"cam.MaxBitrateKbps", cam.MaxBitrateKbps, 4000},
		{"cam.MaxGOP", cam.MaxGOP, 4 * time.Second},
		{"cam.TriggerTimeout", cam.TriggerTimeout, 60 * time.Second},
		{"cam.RecorderQueueFrames", cam.RecorderQueueFrames, 256},
		{"cam.ViewerQueueFrames", cam.ViewerQueueFrames, 64},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestDefaultMasterKeyRef(t *testing.T) {
	p := writeFile(t, t.TempDir(), "c.yaml", `version: 2
relay: { id: "test", max_memory_mb: 768, egress_backlog_mb: 64 }
storage: { driver: "local", root: "zrelay", local: { base_dir: "/x/archive" } }
`)
	_, err := Load(p)
	// The default ref points at a Docker secret that does not exist here.
	if err == nil || !strings.Contains(err.Error(), "encryption.master_key") ||
		!strings.Contains(err.Error(), "/run/secrets/zrelay_master") {
		t.Fatalf("err = %v, want unresolvable default FILE:/run/secrets/zrelay_master", err)
	}
}

func TestCameraOverridesDefaults(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("CAM_USER", "admin")
	t.Setenv("CAM_PASS", "pw")
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfig(`  - id: "front-door"
    brand: "tapo"
    host: "192.0.2.33"
    credentials: { username: "ENV:CAM_USER", password: "ENV:CAM_PASS" }
    rtsp: { path: "/stream1" }
    pre_roll_seconds: 5
    viewer_queue_frames: 32
`)+`defaults:
  post_roll_seconds: 20
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cam := cfg.Cameras[0]
	if cam.PreRoll != 5*time.Second {
		t.Errorf("PreRoll = %v, want 5s (camera override)", cam.PreRoll)
	}
	if cam.ViewerQueueFrames != 32 {
		t.Errorf("ViewerQueueFrames = %d, want 32 (camera override)", cam.ViewerQueueFrames)
	}
	if cam.PostRoll != 20*time.Second {
		t.Errorf("PostRoll = %v, want 20s (from defaults)", cam.PostRoll)
	}
	if cam.MaxClip != 300*time.Second {
		t.Errorf("MaxClip = %v, want 300s (built-in default)", cam.MaxClip)
	}
}

func TestRulesCopiedWithActiveValueDefault(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("CAM_USER", "admin")
	t.Setenv("CAM_PASS", "pw")
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfig(`  - id: "backyard"
    brand: "reolink"
    host: "192.0.2.51"
    credentials: { username: "ENV:CAM_USER", password: "ENV:CAM_PASS" }
    rtsp: { port: 554, path: "/h264Preview_01_main" }
    events:
      source: "onvif"
      port: 8000
      rules:
        - { kind: motion, topic: '(?i)MotionAlarm', item: "State" }
        - { kind: person, topic: '(?i)PeopleDetect', item: "State", active_value: "1" }
`))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []RuleConfig{
		{Kind: "motion", Topic: "(?i)MotionAlarm", Item: "State", ActiveValue: "true"},
		{Kind: "person", Topic: "(?i)PeopleDetect", Item: "State", ActiveValue: "1"},
	}
	got := cfg.Cameras[0].Rules
	if len(got) != len(want) {
		t.Fatalf("Rules = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Rules[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLiteralSecretRejectedWithoutOptIn(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfig(`  - id: "front-door"
    brand: "tapo"
    host: "192.0.2.33"
    credentials: { username: "admin", password: "hunter2-literal" }
    rtsp: { path: "/stream1" }
`))
	_, err := Load(p)
	if !errors.Is(err, secretref.ErrLiteral) {
		t.Fatalf("err = %v, want secretref.ErrLiteral", err)
	}
	if !strings.Contains(err.Error(), "cameras[0].credentials.password") {
		t.Errorf("error %q does not mention cameras[0].credentials.password", err)
	}
	if strings.Contains(err.Error(), "hunter2-literal") {
		t.Errorf("error %q leaks the literal secret", err)
	}
}

func TestEndpointErrorDoesNotLeakValue(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("S3_ENDPOINT", "ftp://user:topsecret@nas:21")
	p := writeFile(t, t.TempDir(), "c.yaml", `version: 2
allow_literal_secrets: true
relay: { id: "test", max_memory_mb: 768, egress_backlog_mb: 64 }
encryption: { master_key: "ENV:ZR_TEST_MASTER" }
storage:
  driver: "s3"
  root: "zrelay"
  s3: { endpoint: "ENV:S3_ENDPOINT", bucket: "cctv", access_key: "AK", secret_key: "SK" }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "storage.s3.endpoint") {
		t.Fatalf("err = %v, want error mentioning storage.s3.endpoint", err)
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Errorf("error %q leaks endpoint userinfo", err)
	}
}

func TestAPITokenHashMustBeHex(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	p := writeFile(t, t.TempDir(), "c.yaml", `version: 2
relay: { id: "test", max_memory_mb: 768, egress_backlog_mb: 64 }
encryption: { master_key: "ENV:ZR_TEST_MASTER" }
storage: { driver: "local", root: "zrelay", local: { base_dir: "/x/archive" } }
live: { listen: "0.0.0.0:9465", advertise_ip: "100.64.0.7", max_viewers: 4, api_token_sha256: ["not-hex"] }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "live.api_token_sha256[0]") {
		t.Fatalf("err = %v, want error mentioning live.api_token_sha256[0]", err)
	}
}

func TestRTSPURLEscapesUserinfo(t *testing.T) {
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("CAM_USER", "ad@min")
	t.Setenv("CAM_PASS", "p:ss/w?rd")
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfig(`  - id: "front-door"
    brand: "tapo"
    host: "192.0.2.33"
    credentials: { username: "ENV:CAM_USER", password: "ENV:CAM_PASS" }
    rtsp: { port: 8554, path: "/stream1" }
`))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.RTSPURL(cfg.Cameras[0])
	want := "rtsp://ad%40min:p%3Ass%2Fw%3Frd@192.0.2.33:8554/stream1"
	if got != want {
		t.Fatalf("RTSPURL = %q, want %q", got, want)
	}
}

func TestRTSPURLIPv6Host(t *testing.T) {
	cfg := &Config{}
	cam := Camera{Host: "fd00::5", Username: "u", Password: "p", RTSP: RTSP{Port: 554, Path: "/stream1"}}
	got := cfg.RTSPURL(cam)
	want := "rtsp://u:p@[fd00::5]:554/stream1"
	if got != want {
		t.Fatalf("RTSPURL = %q, want %q", got, want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load of missing file: want error, got nil")
	}
}
