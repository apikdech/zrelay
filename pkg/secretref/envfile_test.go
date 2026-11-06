package secretref

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertEnv(t *testing.T, k, want string) {
	t.Helper()
	got, ok := os.LookupEnv(k)
	if !ok {
		t.Fatalf("%s is unset, want %q", k, want)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", k, got, want)
	}
}

func TestLoadEnvFileRealEnvWins(t *testing.T) {
	t.Setenv("A", "9")
	unsetenv(t, "B")
	unsetenv(t, "C")

	p := writeEnvFile(t, "A=1\nB=2\n# c\n\nexport C=3")
	if err := LoadEnvFile(p); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	assertEnv(t, "A", "9")
	assertEnv(t, "B", "2")
	assertEnv(t, "C", "3")
}

func TestLoadEnvFileQuotedValues(t *testing.T) {
	unsetenv(t, "D")
	unsetenv(t, "E")
	unsetenv(t, "F")

	p := writeEnvFile(t, "D=\"x y\"\nE='p q'\r\nF=\"unterminated\n")
	if err := LoadEnvFile(p); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	assertEnv(t, "D", "x y")
	assertEnv(t, "E", "p q")
	assertEnv(t, "F", "\"unterminated")
}

func TestLoadEnvFileMalformedLine(t *testing.T) {
	unsetenv(t, "G")

	p := writeEnvFile(t, "G=1\nnot-a-pair-s3cr3t\n")
	err := LoadEnvFile(p)
	if err == nil {
		t.Fatal("LoadEnvFile with malformed line: want error, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error %q does not name line 2", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error %q leaks line content", err)
	}
	if _, ok := os.LookupEnv("G"); ok {
		t.Fatal("G was set although the file failed to parse")
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("LoadEnvFile of missing file: want error, got nil")
	}
}
