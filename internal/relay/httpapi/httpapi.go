// Package httpapi is the relay's operations endpoint (relay-spec §4.12):
// GET /healthz and GET /metrics. It is meant for loopback; the default listen
// address is 127.0.0.1:9464 and an empty one never means "all interfaces".
package httpapi

import (
	"net/http"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/metrics"
)

// DefaultListen is used when New gets an empty listen address
// (config.DefaultHTTPListen).
const DefaultListen = "127.0.0.1:9464"

// New returns the (not yet serving) server for /healthz and /metrics.
// /healthz answers 200 "ok" while health returns nil, else 503 with the
// error text; a nil health is always healthy.
func New(listen string, m *metrics.Metrics, health func() error) *http.Server {
	if listen == "" {
		listen = DefaultListen
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if health != nil {
			if err := health(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("unhealthy: " + err.Error() + "\n"))
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", m.Handler())
	return &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}
