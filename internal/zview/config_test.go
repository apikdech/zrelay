package zview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodHash = "0000000000000000000000000000000000000000000000000000000000000001"

const baseYAML = `version: 2
server:
  listen: "0.0.0.0:8080"
  api_token_sha256: ["ENV:TEST_TOKEN_HASH"]
  app_dir: "/app"
storage:
  driver: "s3"
  root: "zrelay"
  s3:
    endpoint: "ENV:TEST_S3_ENDPOINT"
    region: "us-east-1"
    bucket: "cctv"
    use_path_style: true
    access_key: "ENV:TEST_S3_AK"
    secret_key: "FILE:%SECRET%"
`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "sk")
	if err := os.WriteFile(secret, []byte("s3cr3t-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body = strings.ReplaceAll(body, "%SECRET%", secret)
	p := filepath.Join(dir, "zview.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func setEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_TOKEN_HASH", goodHash)
	t.Setenv("TEST_S3_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("TEST_S3_AK", "ak-value")
}

// inject adds line directly below the first occurrence of header in doc.
func inject(doc, header, line string) string {
	return strings.Replace(doc, header+"\n", header+"\n"+line+"\n", 1)
}

func TestLoadConfigResolvesRefsAndAppliesDefaults(t *testing.T) {
	setEnv(t)
	cfg, err := LoadConfig(writeCfg(t, baseYAML))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Storage.S3.Endpoint != "http://127.0.0.1:9000" || cfg.Storage.S3.AccessKey != "ak-value" || cfg.Storage.S3.SecretKey != "s3cr3t-value" {
		t.Errorf("refs not resolved: %+v", cfg.Storage.S3)
	}
	if len(cfg.Server.APITokenSHA256) != 1 || cfg.Server.APITokenSHA256[0] != goodHash {
		t.Errorf("api_token_sha256 = %v", cfg.Server.APITokenSHA256)
	}
	got := cfg.Index
	if got.HotRefreshSeconds != 30 || got.ColdCacheDays != 60 || got.AssumedMaxClipSeconds != 300 || got.RetentionDays != 30 {
		t.Errorf("index defaults = %+v", got)
	}
	if cfg.Live.Upstream != "http://zrelay:9465" {
		t.Errorf("live.upstream default = %q", cfg.Live.Upstream)
	}
}

func TestLoadConfigRejectsUnknownKeysWithPath(t *testing.T) {
	setEnv(t)
	cases := map[string]string{
		"server.mode":                inject(baseYAML, "server:", "  mode: data"),
		"server.allowed_app_origins": inject(baseYAML, "server:", "  allowed_app_origins: []"),
		"storage.s3.presign":         inject(baseYAML, "  s3:", "    presign: true"),
		"storage.sftp":               inject(baseYAML, "storage:", "  sftp: {}"),
		"index.hot_refresh_secs":     baseYAML + "index:\n  hot_refresh_secs: 3\n",
		"data_origin":                baseYAML + "data_origin: x\n",
	}
	for key, body := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := LoadConfig(writeCfg(t, body))
			if err == nil {
				t.Fatal("accepted an unknown key")
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %q", err, key)
			}
		})
	}
}

func TestLoadConfigLiteralSecrets(t *testing.T) {
	setEnv(t)
	lit := strings.Replace(baseYAML, `access_key: "ENV:TEST_S3_AK"`, `access_key: "plain-literal"`, 1)
	if _, err := LoadConfig(writeCfg(t, lit)); err == nil || !strings.Contains(err.Error(), "storage.s3.access_key") {
		t.Fatalf("literal access_key accepted or unnamed: %v", err)
	}
	ok, err := LoadConfig(writeCfg(t, lit+"allow_literal_secrets: true\n"))
	if err != nil {
		t.Fatalf("allow_literal_secrets: %v", err)
	}
	if ok.Storage.S3.AccessKey != "plain-literal" {
		t.Errorf("access_key = %q", ok.Storage.S3.AccessKey)
	}
	kc := strings.Replace(baseYAML, `access_key: "ENV:TEST_S3_AK"`, `access_key: "KEYCHAIN:zview"`, 1)
	if _, err := LoadConfig(writeCfg(t, kc+"allow_literal_secrets: true\n")); err == nil {
		t.Fatal("KEYCHAIN: accepted")
	}
}

func TestLoadConfigErrorsNeverEchoSecrets(t *testing.T) {
	setEnv(t)
	t.Setenv("TEST_S3_AK", "super-secret-access-key")
	body := strings.Replace(baseYAML, `bucket: "cctv"`, `bucket: ""`, 1)
	_, err := LoadConfig(writeCfg(t, body))
	if err == nil {
		t.Fatal("empty bucket accepted")
	}
	for _, s := range []string{"super-secret-access-key", "s3cr3t-value"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error leaks %q: %v", s, err)
		}
	}
}

func TestLoadConfigValidation(t *testing.T) {
	setEnv(t)
	cases := []struct {
		name, from, to, field string
	}{
		{"version", "version: 2", "version: 1", "version"},
		{"driver", `driver: "s3"`, `driver: "smb"`, "storage.driver"},
		{"bucket", `bucket: "cctv"`, `bucket: ""`, "storage.s3.bucket"},
		{"listen", `listen: "0.0.0.0:8080"`, `listen: "nope"`, "server.listen"},
		{"hash length", `["ENV:TEST_TOKEN_HASH"]`, `["abcd"]`, "server.api_token_sha256[0]"},
		{"no hashes", `["ENV:TEST_TOKEN_HASH"]`, `[]`, "server.api_token_sha256"},
		{"root", `root: "zrelay"`, `root: "../x"`, "storage.root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(baseYAML, c.from, c.to, 1)
			_, err := LoadConfig(writeCfg(t, body))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.field) {
				t.Errorf("error %q does not name %q", err, c.field)
			}
		})
	}
	idx := map[string]string{
		"index.hot_refresh_seconds":      "index:\n  hot_refresh_seconds: 0\n",
		"index.cold_cache_days":          "index:\n  cold_cache_days: -1\n",
		"index.assumed_max_clip_seconds": "index:\n  assumed_max_clip_seconds: 0\n",
		"index.retention_days":           "index:\n  retention_days: -5\n",
		"index.cameras_fallback[1]":      "index:\n  cameras_fallback: [front, \"Bad/Cam\"]\n",
	}
	for field, extra := range idx {
		t.Run(field, func(t *testing.T) {
			_, err := LoadConfig(writeCfg(t, baseYAML+extra))
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("want error naming %q, got %v", field, err)
			}
		})
	}
}

func TestLoadConfigLocalDriver(t *testing.T) {
	dir := t.TempDir()
	body := "version: 2\nserver:\n  listen: \"127.0.0.1:8080\"\n  api_token_sha256: [\"" + goodHash + "\"]\nstorage:\n  driver: local\n  root: zrelay\n  local:\n    base_dir: " + dir + "\n"
	cfg, err := LoadConfig(writeCfg(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Storage.Local.BaseDir != dir {
		t.Errorf("base_dir = %q", cfg.Storage.Local.BaseDir)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/zview.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := strings.ReplaceAll(string(raw), "/run/secrets/", dir+"/")
	for n, v := range map[string]string{"api_token_sha256": goodHash, "s3_ro_secret_key": "secret"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("S3_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("S3_RO_ACCESS_KEY", "ro")
	p := filepath.Join(dir, "zview.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if cfg.Server.Listen != "0.0.0.0:8080" || cfg.Index.RetentionDays != 30 || cfg.Live.Upstream != "http://zrelay:9465" {
		t.Errorf("unexpected example values: %+v", cfg)
	}
}
