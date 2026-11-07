package zview

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

func TestHealthzNeedsNoAuth(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("GET", "/healthz", "-"); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz without auth = %d, want 200", rec.Code)
	}
}

func TestAuthFailuresShareOneUniformBody(t *testing.T) {
	e := newEnv(t)
	valid := strings.TrimPrefix(e.token, "Bearer ")
	bad := []struct{ name, auth string }{
		{"missing", "-"},
		{"padded", "Bearer " + valid + "="},
		{"short", "Bearer " + valid[:20]},
		{"wrong", "Bearer " + strings.Repeat("A", len(valid))},
		{"basic", "Basic " + valid},
		{"empty", "Bearer "},
	}
	var wantBody string
	for i, c := range bad {
		// A distinct client per case keeps the rate limiter out of this test.
		rec := e.doFrom(fmt.Sprintf("198.51.100.%d:1", i+1), "GET", "/api/v1/status", c.auth)
		wantProblem(t, rec, http.StatusUnauthorized)
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s: WWW-Authenticate = %q, want Bearer", c.name, got)
		}
		if i == 0 {
			wantBody = rec.Body.String()
		} else if rec.Body.String() != wantBody {
			t.Errorf("%s: body %q differs from %q", c.name, rec.Body.String(), wantBody)
		}
	}
	if rec := e.do("GET", "/api/v1/status", ""); rec.Code != http.StatusOK {
		t.Errorf("valid token: status = %d, want 200", rec.Code)
	}
}

func TestAuthFailuresAreRateLimitedPerIP(t *testing.T) {
	e := newEnv(t)
	const attacker, other = "203.0.113.9:5000", "203.0.113.10:5000"
	for i := 1; i <= 10; i++ {
		if rec := e.doFrom(attacker, "GET", "/api/v1/status", "Bearer nope"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	rec := e.doFrom(attacker, "GET", "/api/v1/status", "Bearer nope")
	wantProblem(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if rec := e.doFrom(other, "GET", "/api/v1/status", "Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a different IP = %d, want 401", rec.Code)
	}
	// Failures are what is limited: the real key still works from the same
	// address, so a noisy neighbour behind one proxy cannot lock the family out.
	if rec := e.doFrom(attacker, "GET", "/api/v1/status", ""); rec.Code != http.StatusOK {
		t.Errorf("valid token from a limited IP = %d, want 200", rec.Code)
	}
}

// Behind tailscale serve every client arrives from the proxy's address, so the
// peer alone would make the limit global: one stale device would turn every
// other device's 401 into 429. The proxy names the user in Tailscale-User-Login.
func TestAuthFailureLimitIsPerTailscaleUserBehindProxy(t *testing.T) {
	e := newEnv(t)
	const proxy = "127.0.0.1:5000"
	stale := []string{"Tailscale-User-Login", "stale@example.com"}
	for i := 1; i <= 10; i++ {
		if rec := e.doFrom(proxy, "GET", "/api/v1/status", "Bearer nope", stale...); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	wantProblem(t, e.doFrom(proxy, "GET", "/api/v1/status", "Bearer nope", stale...), http.StatusTooManyRequests)
	if rec := e.doFrom(proxy, "GET", "/api/v1/status", "Bearer nope", "Tailscale-User-Login", "other@example.com"); rec.Code != http.StatusUnauthorized {
		t.Errorf("another user behind the same proxy = %d, want 401", rec.Code)
	}
	// A request without the header falls back to the peer address.
	if rec := e.doFrom(proxy, "GET", "/api/v1/status", "Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("headerless request behind the proxy = %d, want 401", rec.Code)
	}
}

// Any client can send the header, so it is believed only from a proxy-like
// peer (loopback or private); from anywhere else it must not buy a fresh
// failure budget.
func TestTailscaleUserHeaderIgnoredFromPublicPeer(t *testing.T) {
	e := newEnv(t)
	const peer = "203.0.113.9:5000"
	for i := 1; i <= 10; i++ {
		hdr := []string{"Tailscale-User-Login", fmt.Sprintf("u%d@example.com", i)}
		if rec := e.doFrom(peer, "GET", "/api/v1/status", "Bearer nope", hdr...); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	rec := e.doFrom(peer, "GET", "/api/v1/status", "Bearer nope", "Tailscale-User-Login", "fresh@example.com")
	wantProblem(t, rec, http.StatusTooManyRequests)
}

func TestLiveRouteIsBehindBearerAuth(t *testing.T) {
	e := newEnv(t)
	wantProblem(t, e.do("POST", "/live/front", "-"), http.StatusUnauthorized)
	if len(e.live.calls) != 0 {
		t.Fatalf("live handler reached without auth: %v", e.live.calls)
	}
	if rec := e.do("DELETE", "/live/front/abc", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("authorized live request = %d, want 204 from the live handler", rec.Code)
	}
	if len(e.live.calls) != 1 || e.live.calls[0] != "DELETE /live/front/abc" {
		t.Errorf("live calls = %v", e.live.calls)
	}
}

func TestFailureLimiterWindow(t *testing.T) {
	clk := clock.NewFake(testNow)
	l := newFailureLimiter(clk, 3, time.Minute)
	for i := 1; i <= 3; i++ {
		if !l.fail("a") {
			t.Fatalf("failure %d limited, want allowed", i)
		}
	}
	if l.fail("a") {
		t.Fatal("4th failure allowed, want limited")
	}
	if !l.fail("b") {
		t.Fatal("other key limited")
	}
	clk.Advance(61 * time.Second)
	if !l.fail("a") {
		t.Fatal("still limited after the window passed")
	}
}

func TestFailureLimiterBoundsMemory(t *testing.T) {
	clk := clock.NewFake(testNow)
	l := newFailureLimiter(clk, 3, time.Minute)
	for i := 0; i < 3*maxLimiterKeys; i++ {
		l.fail(fmt.Sprintf("k%d", i))
	}
	if n := l.size(); n > maxLimiterKeys {
		t.Fatalf("limiter holds %d keys, want <= %d", n, maxLimiterKeys)
	}
}
