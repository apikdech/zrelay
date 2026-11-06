package live

import (
	"testing"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

func TestNewServerValidation(t *testing.T) {
	ver, _ := testToken(t)
	hubs := map[string]*camera.Hub{"cam1": camera.NewHub("cam1")}
	base := func() Config {
		return Config{Listen: "127.0.0.1:0", MaxViewers: 1, Verifier: ver, Hubs: hubs}
	}
	for name, mutate := range map[string]func(*Config){
		"nil verifier":        func(c *Config) { c.Verifier = nil },
		"negative viewers":    func(c *Config) { c.MaxViewers = -1 },
		"nil hub":             func(c *Config) { c.Hubs = map[string]*camera.Hub{"cam1": nil} },
		"bad listen":          func(c *Config) { c.Listen = "nonsense" },
		"listen not an IP":    func(c *Config) { c.Listen = "localhost:0" },
		"advertise not an IP": func(c *Config) { c.AdvertiseIP = "not-an-ip" },
	} {
		cfg := base()
		mutate(&cfg)
		s, err := NewServer(cfg)
		if err == nil {
			s.Close()
			t.Errorf("%s: NewServer succeeded, want an error", name)
		}
	}
}

func TestNewServerDefaults(t *testing.T) {
	ver, _ := testToken(t)
	s, err := NewServer(Config{Listen: "127.0.0.1:0", MaxViewers: 1, Verifier: ver, Hubs: map[string]*camera.Hub{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.cfg.ViewerQueue != 64 {
		t.Errorf("ViewerQueue = %d, want 64", s.cfg.ViewerQueue)
	}
	if s.cfg.Clock != clock.Real {
		t.Errorf("Clock = %v, want clock.Real", s.cfg.Clock)
	}
}

func TestNewServerDisabledOpensNoSocket(t *testing.T) {
	ver, _ := testToken(t)
	s, err := NewServer(Config{MaxViewers: 0, Verifier: ver})
	if err != nil {
		t.Fatal(err)
	}
	if s.conn != nil || s.api != nil {
		t.Fatal("disabled live view opened a socket")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
