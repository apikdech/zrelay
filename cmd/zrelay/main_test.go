package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exampleConfig = "../../deploy/zrelay.example.yaml"

// exampleEnv are the ENV: references of deploy/zrelay.example.yaml.
var exampleEnv = map[string]string{
	"S3_ENDPOINT":     "https://nas.example.ts.net:9000",
	"S3_ACCESS_KEY":   "s3-access",
	"TAILSCALE_IP":    "100.64.0.7",
	"RESTREAM_USER":   "viewer",
	"RESTREAM_PASS":   "restream-pass",
	"CAMERA_IP":       "192.0.2.20",
	"CAMERA_USERNAME": "admin",
	"CAMERA_PASSWORD": "cam-pass",
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// exampleConfigFile writes deploy/zrelay.example.yaml under a temp dir with
// its FILE: secrets redirected there and edit applied, and returns its path.
// The ENV: references are not set.
func exampleConfigFile(t *testing.T, edit func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "zrelay_master", "zk1-example-master-key\n")
	writeFile(t, dir, "s3_secret_key", "s3-secret\n")
	writeFile(t, dir, "api_token_sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n")
	src, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	yml := strings.ReplaceAll(string(src), "FILE:/run/secrets/", "FILE:"+dir+"/")
	if edit != nil {
		yml = edit(yml)
	}
	return writeFile(t, dir, "zrelay.yaml", yml)
}

func setExampleEnv(t *testing.T) {
	t.Helper()
	for k, v := range exampleEnv {
		t.Setenv(k, v)
	}
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestConfigCheckExitCodes(t *testing.T) {
	t.Run("valid example", func(t *testing.T) {
		setExampleEnv(t)
		code, out, errOut := runCLI("config", "check", "--config", exampleConfigFile(t, nil))
		if code != 0 {
			t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
		}
		if !strings.Contains(out, "TOTAL") {
			t.Errorf("stdout has no TOTAL row:\n%s", out)
		}
		if !strings.Contains(out, "tapo-c220") {
			t.Errorf("stdout has no camera row:\n%s", out)
		}
	})

	t.Run("budget over", func(t *testing.T) {
		setExampleEnv(t)
		path := exampleConfigFile(t, func(s string) string {
			return strings.Replace(s, "max_memory_mb: 768", "max_memory_mb: 512", 1)
		})
		code, out, errOut := runCLI("config", "check", "--config", path)
		if code != 1 {
			t.Fatalf("exit %d, want 1; stderr:\n%s", code, errOut)
		}
		if !strings.Contains(errOut, "budget exceeds 80%") {
			t.Errorf("stderr lacks %q:\n%s", "budget exceeds 80%", errOut)
		}
		if !strings.Contains(out, "TOTAL") {
			t.Errorf("the budget table is printed even when over:\n%s", out)
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		setExampleEnv(t)
		path := exampleConfigFile(t, func(s string) string { return s + "retention: { days: 30 }\n" })
		code, _, errOut := runCLI("config", "check", "--config", path)
		if code != 2 {
			t.Fatalf("exit %d, want 2; stderr:\n%s", code, errOut)
		}
		if !strings.Contains(errOut, "retention") {
			t.Errorf("stderr does not name the bad key:\n%s", errOut)
		}
	})

	t.Run("unresolvable reference", func(t *testing.T) {
		setExampleEnv(t)
		os.Unsetenv("CAMERA_PASSWORD") // restored by t.Setenv's cleanup
		code, _, errOut := runCLI("config", "check", "--config", exampleConfigFile(t, nil))
		if code != 2 {
			t.Fatalf("exit %d, want 2; stderr:\n%s", code, errOut)
		}
	})

	t.Run("missing --config", func(t *testing.T) {
		if code, _, _ := runCLI("config", "check"); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})
}

func TestConfigCheckErrorsDoNotLeakSecrets(t *testing.T) {
	setExampleEnv(t)
	path := exampleConfigFile(t, func(s string) string { return s + "mlock: true\n" })
	_, out, errOut := runCLI("config", "check", "--config", path)
	for _, v := range []string{"s3-secret", "s3-access", "cam-pass", "restream-pass", "zk1-example-master-key"} {
		if strings.Contains(out+errOut, v) {
			t.Errorf("output contains secret %q:\n%s%s", v, out, errOut)
		}
	}
}

func TestEnvFileLoadsReferences(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for k, v := range exampleEnv {
		if _, set := os.LookupEnv(k); set {
			t.Skipf("%s is set in the real environment", k)
		}
		lines = append(lines, k+"="+v)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	envFile := writeFile(t, dir, "zrelay.env", strings.Join(lines, "\n")+"\n")
	path := exampleConfigFile(t, nil)

	if code, _, errOut := runCLI("--env-file", envFile, "config", "check", "--config", path); code != 0 {
		t.Fatalf("global --env-file: exit %d, want 0; stderr:\n%s", code, errOut)
	}
	for k := range exampleEnv {
		os.Unsetenv(k)
	}
	if code, _, errOut := runCLI("config", "check", "--config", path, "--env-file", envFile); code != 0 {
		t.Fatalf("subcommand --env-file: exit %d, want 0; stderr:\n%s", code, errOut)
	}
}

func TestEnvFileMissingFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.env")
	if code, _, _ := runCLI("--env-file", path, "config", "check", "--config", exampleConfig); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"config"}, {"config", "bogus"}} {
		if code, _, errOut := runCLI(args...); code != 2 || errOut == "" {
			t.Errorf("run(%q) = %d with stderr %q, want 2 with usage", args, code, errOut)
		}
	}
}
