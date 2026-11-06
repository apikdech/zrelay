package secretref

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordHandler is a slog.Handler that keeps every record it receives.
type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

// captureLogs installs a recording default logger for the duration of t.
func captureLogs(t *testing.T) *recordHandler {
	t.Helper()
	h := &recordHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// recordText renders a record's message and attributes for content checks.
func recordText(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	return b.String()
}

// unsetenv unsets k for the duration of t and restores it afterwards.
func unsetenv(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "")
	if err := os.Unsetenv(k); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// Chmod explicitly so the process umask cannot tighten the mode.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveEnv(t *testing.T) {
	t.Setenv("FOO", "bar")
	got, err := Resolve("ENV:FOO", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "bar" {
		t.Fatalf("Resolve = %q, want %q", got, "bar")
	}

	t.Setenv("ZRELAY_SECRETREF_OTHER", "s3cr3t-value")
	unsetenv(t, "FOO")
	_, err = Resolve("ENV:FOO", false)
	if err == nil {
		t.Fatal("Resolve of unset variable: want error, got nil")
	}
	if !strings.Contains(err.Error(), "FOO") {
		t.Fatalf("error %q does not name FOO", err)
	}
	for _, v := range []string{"bar", "s3cr3t-value"} {
		if strings.Contains(err.Error(), v) {
			t.Fatalf("error %q leaks value %q", err, v)
		}
	}
}

func TestResolveFileTrimsOneTrailingNewline(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{"k\n", "k"},
		{"k\n\n", "k\n"},
		{"k", "k"},
	} {
		p := writeFile(t, tc.content, 0o600)
		got, err := Resolve("FILE:"+p, false)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.content, err)
		}
		if got != tc.want {
			t.Fatalf("Resolve(file %q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}

func TestResolveFileWarnsOnLooseMode(t *testing.T) {
	const secret = "file-secret-xyz"

	h := captureLogs(t)
	p := writeFile(t, secret+"\n", 0o644)
	if _, err := Resolve("FILE:"+p, false); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var warns []slog.Record
	for _, r := range h.records {
		if r.Level == slog.LevelWarn {
			warns = append(warns, r)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("got %d warnings for mode 0644, want 1", len(warns))
	}
	if txt := recordText(warns[0]); strings.Contains(txt, secret) {
		t.Fatalf("warning %q leaks the secret", txt)
	}

	h = captureLogs(t)
	p = writeFile(t, secret+"\n", 0o600)
	if _, err := Resolve("FILE:"+p, false); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(h.records) != 0 {
		t.Fatalf("got %d log records for mode 0600, want 0", len(h.records))
	}
}

func TestResolveFileMissing(t *testing.T) {
	_, err := Resolve("FILE:"+filepath.Join(t.TempDir(), "absent"), false)
	if err == nil {
		t.Fatal("Resolve of missing file: want error, got nil")
	}
}

func TestResolveRejectsLiteral(t *testing.T) {
	_, err := Resolve("hunter2", false)
	if !errors.Is(err, ErrLiteral) {
		t.Fatalf("Resolve literal with allowLiteral=false: err = %v, want ErrLiteral", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %q leaks the literal", err)
	}

	got, err := Resolve("hunter2", true)
	if err != nil {
		t.Fatalf("Resolve literal with allowLiteral=true: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("Resolve = %q, want %q", got, "hunter2")
	}
}

func TestResolveRejectsKeychain(t *testing.T) {
	for _, allow := range []bool{false, true} {
		_, err := Resolve("KEYCHAIN:a/b", allow)
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Resolve KEYCHAIN (allowLiteral=%v): err = %v, want ErrUnsupported", allow, err)
		}
		if strings.Contains(err.Error(), "a/b") {
			t.Fatalf("error %q leaks the reference body", err)
		}
	}
}

func TestResolveRejectsOtherSchemes(t *testing.T) {
	_, err := Resolve("VAULT:x", true)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resolve VAULT: err = %v, want ErrUnsupported", err)
	}
}

func TestIsRef(t *testing.T) {
	for s, want := range map[string]bool{
		"ENV:FOO":       true,
		"FILE:/run/x":   true,
		"hunter2":       false,
		"192.168.1.10":  false,
		"KEYCHAIN:a/b":  false,
		"env:foo":       false,
		"":              false,
		"rtsp://h:554/": false,
	} {
		if got := IsRef(s); got != want {
			t.Errorf("IsRef(%q) = %v, want %v", s, got, want)
		}
	}
}
