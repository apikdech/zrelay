package httpapi_test

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/httpapi"
	"github.com/apikdech/zrelay-v2/internal/relay/metrics"
)

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	var healthErr error
	sv := httpapi.New("127.0.0.1:0", m, func() error { return healthErr })

	if rec := do(sv.Handler, "GET", "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthy: /healthz = %d, want 200", rec.Code)
	}
	healthErr = errors.New("storage breaker open for 6m0s")
	rec := do(sv.Handler, "GET", "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy: /healthz = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "storage breaker open") {
		t.Errorf("503 body %q should say why", rec.Body.String())
	}
}

func TestHealthzNilCheckIsHealthy(t *testing.T) {
	sv := httpapi.New("127.0.0.1:0", metrics.New(prometheus.NewRegistry()), nil)
	if rec := do(sv.Handler, "GET", "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RegisterCamera("cam1", metrics.CameraSources{})
	m.UploadBytes(42)
	sv := httpapi.New("127.0.0.1:0", m, nil)
	rec := do(sv.Handler, "GET", "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{"zrelay_upload_bytes_total 42", "go_goroutines", `zrelay_camera_rtsp_up{camera="cam1"} 0`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

func TestOnlyGETIsServed(t *testing.T) {
	sv := httpapi.New("127.0.0.1:0", metrics.New(prometheus.NewRegistry()), nil)
	for _, p := range []string{"/healthz", "/metrics"} {
		if rec := do(sv.Handler, "POST", p); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", p, rec.Code)
		}
	}
	if rec := do(sv.Handler, "GET", "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

// http_listen defaults to loopback (relay-spec §4.12): an empty listen string
// must never fall back to all interfaces.
func TestHTTPListenDefaultLoopback(t *testing.T) {
	sv := httpapi.New("", metrics.New(prometheus.NewRegistry()), nil)
	if sv.Addr != "127.0.0.1:9464" {
		t.Fatalf("default Addr = %q, want 127.0.0.1:9464", sv.Addr)
	}
	if httpapi.DefaultListen != config.DefaultHTTPListen {
		t.Errorf("DefaultListen %q != config default %q", httpapi.DefaultListen, config.DefaultHTTPListen)
	}
	host, _, err := net.SplitHostPort(sv.Addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Errorf("default %q is not a loopback address", sv.Addr)
	}
	if got := httpapi.New("127.0.0.1:12345", metrics.New(prometheus.NewRegistry()), nil).Addr; got != "127.0.0.1:12345" {
		t.Errorf("explicit listen not kept: %q", got)
	}
}
