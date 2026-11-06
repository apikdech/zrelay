package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tokenHash = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

func writeConfig(t *testing.T, listen string) string {
	t.Helper()
	dir := t.TempDir()
	app := filepath.Join(dir, "app")
	if err := os.Mkdir(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "index.html"), []byte("<title>zview</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	if err := os.Mkdir(store, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`version: 2
server:
  listen: %q
  api_token_sha256: [%q]
  app_dir: %q
storage:
  driver: local
  root: zrelay
  local:
    base_dir: %q
`, listen, tokenHash, app, store)
	p := filepath.Join(dir, "zview.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUsageErrorsExit2(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:0")
	for name, args := range map[string][]string{
		"no config":      {},
		"unknown flag":   {"--nope"},
		"mode app":       {"--config", cfg, "--mode", "app"},
		"mode data":      {"--config", cfg, "--mode", "data"},
		"missing config": {"--config", filepath.Join(t.TempDir(), "absent.yaml")},
		"extra argument": {"--config", cfg, "extra"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb, nil); code != 2 {
			t.Errorf("%s: exit = %d, want 2 (stderr %q)", name, code, errb.String())
		}
	}
}

func TestInvalidConfigExits2WithoutEchoingSecrets(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:0")
	b, _ := os.ReadFile(cfg)
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(b), tokenHash, "not-a-hash-s3cr3t", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"--config", cfg}, &out, &errb, nil); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if strings.Contains(errb.String(), "s3cr3t") {
		t.Errorf("stderr echoes the config value: %q", errb.String())
	}
}

func TestServesAndShutsDownOnContextCancel(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() {
		done <- run(ctx, []string{"--config", cfg, "--mode", "both"}, &bytes.Buffer{}, &errb, func(addr string) { addrCh <- addr })
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case code := <-done:
		t.Fatalf("run returned %d before listening: %s", code, errb.String())
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("addr = %q", addr)
	}

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := get("/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d", resp.StatusCode)
	}
	if resp := get("/api/v1/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/v1/status without a token = %d, want 401", resp.StatusCode)
	}
	if resp := get("/"); resp.StatusCode != http.StatusOK {
		t.Errorf("/ = %d, want the SPA", resp.StatusCode)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit = %d after shutdown, want 0 (stderr %q)", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after the context was cancelled")
	}
}

// The live proxy is mounted: an authorized offer reaches live.upstream.
func TestMountsLiveProxyOnConfiguredUpstream(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32)
	sum := sha256.Sum256(raw)
	auth := "Bearer " + base64.RawURLEncoding.EncodeToString(raw)

	var gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody = r.Header.Get("Authorization"), string(b)
		w.Header().Set("Location", "/live/front/s1")
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	cfg := writeConfig(t, "127.0.0.1:0")
	b, _ := os.ReadFile(cfg)
	b = []byte(strings.Replace(string(b), tokenHash, hex.EncodeToString(sum[:]), 1))
	b = append(b, []byte("live:\n  upstream: "+upstream.URL+"\n")...)
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() {
		done <- run(ctx, []string{"--config", cfg}, &bytes.Buffer{}, &errb, func(addr string) { addrCh <- addr })
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case code := <-done:
		t.Fatalf("run returned %d before listening: %s", code, errb.String())
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}

	req, _ := http.NewRequest("POST", "http://"+addr+"/live/front", strings.NewReader("v=0\r\n"))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/live/front/s1" {
		t.Errorf("POST /live/front = %d, Location %q; want 201 from the upstream", resp.StatusCode, resp.Header.Get("Location"))
	}
	if gotAuth != auth || gotBody != "v=0\r\n" {
		t.Errorf("upstream saw auth %q body %q", gotAuth, gotBody)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop")
	}
}
