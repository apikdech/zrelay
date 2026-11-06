package zview

import (
	"net/http"
	"testing"
)

// wantCSP is ui-spec §2.4 with design §7.1 applied: connect-src 'self', no
// other origin.
const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' blob: data:; media-src 'self' blob:; worker-src 'self'; connect-src 'self'; " +
	"manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func TestCSPEnforced(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ name, path, token string }{
		{"app shell", "/", "-"},
		{"service worker", "/sw.js", "-"},
		{"health", "/healthz", "-"},
		{"api ok", "/api/v1/cameras", ""},
		{"api 401", "/api/v1/cameras", "-"},
		{"unknown path", "/nope.js", "-"},
	} {
		rec := e.do("GET", c.path, c.token)
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s (%d): Content-Security-Policy = %q, want %q", c.name, rec.Code, got, wantCSP)
		}
		if got := h.Get("Content-Security-Policy-Report-Only"); got != "" {
			t.Errorf("%s: Report-Only header still set: %q", c.name, got)
		}
	}
}

func TestCSPEnforcedOnMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodPost, "/", "-")
	if rec.Header().Get("Content-Security-Policy") != wantCSP {
		t.Errorf("CSP missing on %d response: %v", rec.Code, rec.Header())
	}
}
