package zview

import (
	"net/http"
	"strings"
	"testing"
)

// The PWA manifest is served as application/manifest+json (Task 41), not
// whatever the host's MIME database or content sniffing makes of it.
func TestAppServesManifestWithItsMediaType(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/manifest.webmanifest", "-")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("Content-Type = %q, want application/manifest+json", ct)
	}
}
