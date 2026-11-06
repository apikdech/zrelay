package zview

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/apikdech/zrelay-v2/pkg/sidecar"
)

// srvEnv is a Server over a fixture tree with one valid bearer token.
type srvEnv struct {
	t      *testing.T
	f      *fixture
	cr     *countingReader
	h      http.Handler
	token  string // the Authorization header value
	appDir string
	live   *liveRecorder
}

// liveRecorder is the stand-in for the live proxy handler.
type liveRecorder struct {
	calls []string
}

func (l *liveRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.calls = append(l.calls, r.Method+" "+r.URL.Path)
	w.WriteHeader(http.StatusNoContent)
}

func testToken() (header, hashHex string) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	sum := sha256.Sum256(raw)
	return "Bearer " + base64.RawURLEncoding.EncodeToString(raw), hex.EncodeToString(sum[:])
}

func newEnv(t *testing.T) *srvEnv {
	t.Helper()
	f := newFixture(t)
	cr := f.counting()
	hdr, hash := testToken()
	appDir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(appDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<!doctype html><title>zview</title>")
	write("sw.js", "// service worker")
	write("manifest.webmanifest", "{}")
	write("assets/app-3f9a1c.js", "console.log('app')")
	live := &liveRecorder{}
	cfg := &Config{
		Version: 2,
		Server:  ServerConfig{Listen: "127.0.0.1:0", APITokenSHA256: []string{hash}, AppDir: appDir},
		Index:   IndexConfig{AssumedMaxClipSeconds: 300, CamerasFallback: []string{"front", "back"}},
	}
	idx := f.index(cr, cfg.Index)
	return &srvEnv{t: t, f: f, cr: cr, h: NewServer(cfg, idx, cr, live), token: hdr, appDir: appDir, live: live}
}

// do runs one request from a fixed client. auth "" sends the valid token;
// "-" sends none.
func (e *srvEnv) do(method, target, auth string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.doFrom("192.0.2.1:4000", method, target, auth, hdr...)
}

func (e *srvEnv) doFrom(remote, method, target, auth string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remote
	switch auth {
	case "":
		req.Header.Set("Authorization", e.token)
	case "-":
	default:
		req.Header.Set("Authorization", auth)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// videoBytes returns the stored ciphertext of sc.
func (e *srvEnv) videoBytes(sc *sidecar.Sidecar) []byte {
	e.t.Helper()
	key := sidecar.ClipKeys(sc.Camera.ID, sc.StartTime.Time, sc.ClipID).Video
	b, err := os.ReadFile(filepath.Join(e.f.base, testRoot, filepath.FromSlash(key)))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
}

func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
	}
	decodeJSON(t, rec, &p)
	if p.Status != status || p.Title == "" || p.Type == "" {
		t.Errorf("problem = %+v, want status %d with type and title", p, status)
	}
}
