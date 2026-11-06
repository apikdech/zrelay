package zview

import (
	"net/http"
	"strings"
	"testing"
)

func TestAppSecurityHeadersOnRoot(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/", "-") // the SPA shell needs no token
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	h := rec.Header()
	// Orchestrator note: Report-Only until Task 42 enforces the policy.
	if h.Get("Content-Security-Policy") != "" {
		t.Error("enforcing Content-Security-Policy set; it must stay Report-Only until Task 42")
	}
	csp := h.Get("Content-Security-Policy-Report-Only")
	for _, want := range []string{
		"default-src 'none'", "script-src 'self'", "style-src 'self' 'unsafe-inline'",
		"img-src 'self' blob: data:", "media-src 'self' blob:", "worker-src 'self'",
		"connect-src 'self';", "manifest-src 'self'", "base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "https:") || strings.Contains(csp, "DATA_ORIGIN") {
		t.Errorf("CSP names another origin (single origin, D18): %s", csp)
	}
	for k, want := range map[string]string{
		"Cross-Origin-Opener-Policy": "same-origin",
		"Referrer-Policy":            "no-referrer",
		"Permissions-Policy":         "camera=(self), microphone=(), geolocation=()",
		"X-Content-Type-Options":     "nosniff",
		"Strict-Transport-Security":  "max-age=31536000",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestSecurityHeadersAlsoOnAPIAndErrors(t *testing.T) {
	e := newEnv(t)
	for _, rec := range []interface{ Header() http.Header }{
		e.do("GET", "/api/v1/cameras", ""),
		e.do("GET", "/api/v1/cameras", "-"),
		e.do("GET", "/healthz", "-"),
	} {
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy-Report-Only") == "" {
			t.Errorf("security headers missing: %v", rec.Header())
		}
	}
}

func TestAppCachePolicy(t *testing.T) {
	e := newEnv(t)
	cases := []struct{ path, cache string }{
		{"/", "no-cache"},
		{"/index.html", "no-cache"},
		{"/sw.js", "no-cache"},
		{"/manifest.webmanifest", "no-cache"},
		{"/assets/app-3f9a1c.js", "public, max-age=31536000, immutable"},
	}
	for _, c := range cases {
		rec := e.do("GET", c.path, "-")
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d", c.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != c.cache {
			t.Errorf("%s Cache-Control = %q, want %q", c.path, got, c.cache)
		}
	}
	if ct := e.do("GET", "/assets/app-3f9a1c.js", "-").Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset Content-Type = %q", ct)
	}
}

func TestAppFallsBackToIndexForClientRoutes(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/timeline/2026-11-06", "-")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>zview</title>") {
		t.Fatalf("client route = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("fallback Cache-Control = %q", got)
	}
}

func TestAppDoesNotServeOutsideAppDir(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{
		"/assets/missing.js", // a missing file with an extension is a 404, not index.html
		"/..%2f..%2fetc%2fpasswd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/assets/..%2f..%2fetc%2fpasswd",
		"/assets/",
	} {
		rec := e.do("GET", p, "-")
		if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "<title>zview</title>") {
			t.Errorf("%s served %q", p, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("%s leaked a host file", p)
		}
	}
	if rec := e.do("GET", "/assets/missing.js", "-"); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset = %d, want 404", rec.Code)
	}
}

func TestAppNonGETIsRejected(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("POST", "/", "-"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
}
