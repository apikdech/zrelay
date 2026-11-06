package zview

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/apikdech/zrelay-v2/internal/bearer"
)

const (
	// maxLiveOfferBytes caps a WHEP offer (design §7.1).
	maxLiveOfferBytes = 64 << 10
	// liveHeaderTimeout bounds the wait for zrelay's answer; the upstream
	// gathers ICE candidates for up to 5 s before it answers.
	liveHeaderTimeout = 15 * time.Second
)

// NewLiveProxy returns the handler for zview's live routes (design §7.1):
//
//	POST   /live/{camera}            application/sdp offer, at most 64 KiB
//	DELETE /live/{camera}/{session}
//
// Nothing else is proxied. The bearer credential is checked here, before
// anything reaches the upstream, and the Authorization header is then
// forwarded for the upstream to check again. Only Authorization and
// Content-Type are forwarded. The upstream's status (503 for "max viewers"
// included) and body pass through, and its Location header is rewritten to
// the proxy's own path, so the internal address never reaches a client.
//
// upstream is the base URL of zrelay's live server (cfg.Live.Upstream). It
// panics if upstream is not an http(s) URL without userinfo, which
// LoadConfig already rejects.
func NewLiveProxy(upstream string, v *bearer.Verifier) http.Handler {
	target, err := url.Parse(upstream)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		panic("zview: NewLiveProxy: upstream must be an http(s) URL without userinfo")
	}
	basePath := strings.TrimRight(target.EscapedPath(), "/")

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.RawQuery = ""
			pr.Out.Header = http.Header{}
			for _, h := range []string{"Authorization", "Content-Type"} {
				if val := pr.In.Header.Get(h); val != "" {
					pr.Out.Header.Set(h, val)
				}
			}
		},
		Transport: newLiveTransport(),
		ModifyResponse: func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", proxyLocation(loc, resp.Request.URL, basePath))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The error text names the upstream address, so it stays in the log.
			slog.Warn("live upstream request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			writeProblem(w, r, http.StatusBadGateway, "")
		},
	}

	checked := func(next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !v.Check(r.Header.Get("Authorization")) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeProblem(w, r, http.StatusUnauthorized, "")
				return
			}
			next(w, r)
		})
	}
	mux := http.NewServeMux()
	mux.Handle("POST /live/{camera}", checked(func(w http.ResponseWriter, r *http.Request) {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/sdp" {
			writeProblem(w, r, http.StatusUnsupportedMediaType, "Content-Type must be application/sdp")
			return
		}
		if r.ContentLength > maxLiveOfferBytes {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLiveOfferBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeProblem(w, r, http.StatusRequestEntityTooLarge, "")
			} else {
				writeProblem(w, r, http.StatusBadRequest, "")
			}
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		rp.ServeHTTP(w, r)
	}))
	mux.Handle("DELETE /live/{camera}/{session}", checked(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.NoBody
		r.ContentLength = 0
		rp.ServeHTTP(w, r)
	}))
	return mux
}

// newLiveTransport returns the upstream transport. It never uses a proxy
// from the environment: the offer and the forwarded K_api go straight to
// zrelay.
func newLiveTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.ResponseHeaderTimeout = liveHeaderTimeout
	return tr
}

// proxyLocation turns the upstream's Location into the proxy's path: scheme,
// host and the upstream's own path prefix are dropped. A relative reference
// is first resolved against reqURL, the URL the proxy requested.
func proxyLocation(loc string, reqURL *url.URL, basePath string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return "/live/"
	}
	if reqURL != nil {
		u = reqURL.ResolveReference(u)
	}
	p := u.EscapedPath()
	if basePath != "" && strings.HasPrefix(p, basePath+"/") {
		p = strings.TrimPrefix(p, basePath)
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}
