package config

import (
	"strings"
	"testing"
)

const liveTestCamera = `  - id: "front-door"
    brand: "tapo"
    host: "192.0.2.33"
    credentials: { username: "ENV:CAM_USER", password: "ENV:CAM_PASS" }
    rtsp: { path: "/stream1" }
`

func liveTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ZR_TEST_MASTER", "zk1-x")
	t.Setenv("CAM_USER", "admin")
	t.Setenv("CAM_PASS", "pw")
	t.Setenv("RS_USER", "viewer")
	t.Setenv("RS_PASS", "pw")
}

// Design §5.1/§5.2: live.listen, live.ice_udp_port, live.max_viewers and
// restream.listen have documented defaults, so omitting them must not
// silently disable the features.
func TestLiveRestreamDefaultsApplied(t *testing.T) {
	liveTestEnv(t)
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfigWith(
		`live: { advertise_ip: "100.64.0.7", api_token_sha256: ["`+testTokenHash+`"] }
restream: { username: "ENV:RS_USER", password: "ENV:RS_PASS" }
`, liveTestCamera))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Live.Listen", cfg.Live.Listen, "0.0.0.0:9465"},
		{"Live.ICEUDPPort", cfg.Live.ICEUDPPort, 8189},
		{"Live.MaxViewers", cfg.Live.MaxViewers, 4},
		{"Restream.Listen", cfg.Restream.Listen, ":8554"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// With the defaults applied, a config that omits the live and restream
// blocks is no longer silently feature-less: it fails with a message that
// names what is missing.
func TestOmittedLiveRestreamBlocksFailValidation(t *testing.T) {
	liveTestEnv(t)
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfigWith("", liveTestCamera))
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load: want error for omitted live/restream blocks, got nil")
	}
	for _, path := range []string{"live.advertise_ip", "live.api_token_sha256", "restream.username", "restream.password"} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q does not mention %q", err, path)
		}
	}
}

// Both features can still be turned off explicitly.
func TestLiveRestreamExplicitlyDisabled(t *testing.T) {
	liveTestEnv(t)
	p := writeFile(t, t.TempDir(), "c.yaml", minimalConfig(liveTestCamera))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Live.MaxViewers != 0 || cfg.Restream.Listen != "" {
		t.Fatalf("live=%+v restream=%+v, want both disabled", cfg.Live, cfg.Restream)
	}
}
