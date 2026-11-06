package zview

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/apikdech/zrelay-v2/internal/bearer"
)

const sdpOffer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"

// upstreamCall is one request the fake zrelay live server received.
type upstreamCall struct {
	Method, Path, Auth, ContentType, Cookie, Body string
	ContentLength                                 int64
}

// fakeUpstream stands in for zrelay's live signaling server.
type fakeUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []upstreamCall
	// respond, when set, writes the response; the default is a WHEP 201.
	respond func(w http.ResponseWriter, r *http.Request)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.calls = append(u.calls, upstreamCall{
			Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"), Cookie: r.Header.Get("Cookie"),
			Body: string(body), ContentLength: r.ContentLength,
		})
		respond := u.respond
		u.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/sdp")
			w.Header().Set("Location", "/live/front/sess1")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("answer-sdp"))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) got() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

func (u *fakeUpstream) setRespond(f func(w http.ResponseWriter, r *http.Request)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.respond = f
}

func newProxy(t *testing.T, upstream string) (h http.Handler, authHeader string) {
	t.Helper()
	hdr, hash := testToken()
	v, err := bearer.New([]string{hash})
	if err != nil {
		t.Fatal(err)
	}
	return NewLiveProxy(upstream, v), hdr
}

func proxyDo(h http.Handler, method, target, auth, contentType string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLiveProxyForwardsBodyAndAuth(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)

	req := httptest.NewRequest("POST", "/live/front", strings.NewReader(sdpOffer))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Cookie", "session=secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "answer-sdp" {
		t.Errorf("body = %q, want the upstream answer", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/sdp" {
		t.Errorf("Content-Type = %q", ct)
	}
	calls := up.got()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.Method != "POST" || c.Path != "/live/front" {
		t.Errorf("upstream saw %s %s", c.Method, c.Path)
	}
	if c.Body != sdpOffer || c.ContentLength != int64(len(sdpOffer)) {
		t.Errorf("upstream body = %q (length %d)", c.Body, c.ContentLength)
	}
	if c.Auth != auth {
		t.Errorf("upstream Authorization = %q, want the client's header passed through", c.Auth)
	}
	if c.ContentType != "application/sdp" {
		t.Errorf("upstream Content-Type = %q", c.ContentType)
	}
	if c.Cookie != "" {
		t.Errorf("upstream got Cookie %q; only Authorization and Content-Type are forwarded", c.Cookie)
	}
}

func TestLiveProxyRejectsBadAuthWithoutContactingUpstream(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)
	wrong := "Bearer " + strings.Repeat("A", 43)

	for _, tc := range []struct{ name, method, target, auth, ct string }{
		{"missing", "POST", "/live/front", "", "application/sdp"},
		{"wrong token", "POST", "/live/front", wrong, "application/sdp"},
		{"not bearer", "POST", "/live/front", strings.Replace(auth, "Bearer", "Basic", 1), "application/sdp"},
		{"delete missing", "DELETE", "/live/front/sess1", "", ""},
		{"delete wrong", "DELETE", "/live/front/sess1", wrong, ""},
		// Auth comes first: a bad-token request with a bad media type is 401, not 415.
		{"bad token and bad type", "POST", "/live/front", wrong, "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := proxyDo(h, tc.method, tc.target, tc.auth, tc.ct, strings.NewReader(sdpOffer))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream contacted %d times without valid auth", n)
	}
}

func TestLiveProxyRewritesLocation(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)

	for _, tc := range []struct{ name, upstreamLoc, want string }{
		{"relative", "/live/front/sess1", "/live/front/sess1"},
		{"absolute internal URL", up.srv.URL + "/live/front/sess1", "/live/front/sess1"},
		{"absolute with port and escaping", "http://zrelay:9465/live/a%20b/sess1", "/live/a%20b/sess1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up.setRespond(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", tc.upstreamLoc)
				w.WriteHeader(http.StatusCreated)
			})
			rec := proxyDo(h, "POST", "/live/front", auth, "application/sdp", strings.NewReader(sdpOffer))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != tc.want {
				t.Errorf("Location = %q, want %q", loc, tc.want)
			}
		})
	}
}

func TestLiveProxyRewritesLocationUnderUpstreamPathPrefix(t *testing.T) {
	up := newFakeUpstream(t)
	up.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", r.URL.Path+"/sess1")
		w.WriteHeader(http.StatusCreated)
	})
	h, auth := newProxy(t, up.srv.URL+"/base")
	rec := proxyDo(h, "POST", "/live/front", auth, "application/sdp", strings.NewReader(sdpOffer))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := up.got()[0].Path; got != "/base/live/front" {
		t.Errorf("upstream path = %q, want /base/live/front", got)
	}
	if loc := rec.Header().Get("Location"); loc != "/live/front/sess1" {
		t.Errorf("Location = %q, want the upstream prefix stripped", loc)
	}
}

func TestLiveProxyDeleteIsForwarded(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)
	rec := proxyDo(h, "DELETE", "/live/front/sess1", auth, "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	calls := up.got()
	if len(calls) != 1 || calls[0].Method != "DELETE" || calls[0].Path != "/live/front/sess1" || calls[0].Auth != auth {
		t.Errorf("upstream calls = %+v", calls)
	}
}

func TestLiveProxyRejectsOversizedBody(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)
	big := strings.Repeat("a", 64<<10+1)

	t.Run("declared length", func(t *testing.T) {
		rec := proxyDo(h, "POST", "/live/front", auth, "application/sdp", strings.NewReader(big))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
	})
	t.Run("unknown length", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/live/front", io.NopCloser(strings.NewReader(big)))
		req.ContentLength = -1
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/sdp")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
	})
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream contacted %d times for oversized bodies", n)
	}

	t.Run("exactly 64 KiB is accepted", func(t *testing.T) {
		rec := proxyDo(h, "POST", "/live/front", auth, "application/sdp", strings.NewReader(strings.Repeat("a", 64<<10)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		if c := up.got(); len(c) != 1 || len(c[0].Body) != 64<<10 {
			t.Errorf("upstream calls = %d", len(c))
		}
	})
}

func TestLiveProxyRejectsOtherMediaTypes(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)
	for _, ct := range []string{"", "text/plain", "application/json", "application/sdpx", "garbage;;"} {
		rec := proxyDo(h, "POST", "/live/front", auth, ct, strings.NewReader(sdpOffer))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: status = %d, want 415", ct, rec.Code)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream contacted %d times for wrong media types", n)
	}
	// Parameters and case do not matter.
	rec := proxyDo(h, "POST", "/live/front", auth, "Application/SDP; charset=utf-8", strings.NewReader(sdpOffer))
	if rec.Code != http.StatusCreated {
		t.Errorf("application/sdp with parameters: status = %d, want 201", rec.Code)
	}
}

func TestLiveProxyOnlyPostAndDelete(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/live/front"},
		{"HEAD", "/live/front"},
		{"PUT", "/live/front"},
		{"PATCH", "/live/front"},
		{"DELETE", "/live/front"},
		{"OPTIONS", "/live/front"},
		{"GET", "/live/front/sess1"},
		{"POST", "/live/front/sess1"},
		{"PATCH", "/live/front/sess1"},
	} {
		rec := proxyDo(h, tc.method, tc.target, auth, "application/sdp", strings.NewReader(sdpOffer))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405", tc.method, tc.target, rec.Code)
		}
		if rec.Header().Get("Allow") == "" {
			t.Errorf("%s %s: no Allow header", tc.method, tc.target)
		}
	}
	for _, target := range []string{"/", "/live", "/live/", "/live/front/sess1/extra", "/api/v1/status"} {
		if rec := proxyDo(h, "POST", target, auth, "application/sdp", strings.NewReader(sdpOffer)); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: status = %d, want 404", target, rec.Code)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("upstream contacted %d times for unsupported requests", n)
	}
}

func TestLiveProxyPassesUpstreamStatusThrough(t *testing.T) {
	up := newFakeUpstream(t)
	h, auth := newProxy(t, up.srv.URL)
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusServiceUnavailable, "max viewers reached\n"},
		{http.StatusNotFound, "unknown camera\n"},
		{http.StatusNotImplemented, "live view carries H.264 only\n"},
	} {
		up.setRespond(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		})
		rec := proxyDo(h, "POST", "/live/front", auth, "application/sdp", strings.NewReader(sdpOffer))
		if rec.Code != tc.status || rec.Body.String() != tc.body {
			t.Errorf("upstream %d: got %d %q", tc.status, rec.Code, rec.Body.String())
		}
	}
}

func TestLiveProxyUpstreamDownIs502(t *testing.T) {
	up := newFakeUpstream(t)
	url := up.srv.URL
	up.srv.Close()
	h, auth := newProxy(t, url)
	rec := proxyDo(h, "POST", "/live/front", auth, "application/sdp", strings.NewReader(sdpOffer))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("502 body leaks the upstream address: %q", rec.Body.String())
	}
}

func TestNewLiveProxyPanicsOnBadUpstream(t *testing.T) {
	_, hash := testToken()
	v, _ := bearer.New([]string{hash})
	for _, u := range []string{"", "zrelay:9465", "ftp://zrelay", "http://user:pw@zrelay:9465"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewLiveProxy(%q) did not panic", u)
				}
			}()
			NewLiveProxy(u, v)
		}()
	}
}

// The proxy is mounted behind NewServer's rate-limited auth wrapper.
func TestLiveProxyMountedInServer(t *testing.T) {
	up := newFakeUpstream(t)
	e := newEnv(t)
	hdr, hash := testToken()
	v, _ := bearer.New([]string{hash})
	cfg := &Config{
		Version: 2,
		Server:  ServerConfig{Listen: "127.0.0.1:0", APITokenSHA256: []string{hash}, AppDir: e.appDir},
	}
	h := NewServer(cfg, e.f.index(e.cr, cfg.Index), e.cr, NewLiveProxy(up.srv.URL, v))

	rec := proxyDo(h, "POST", "/live/front", "", "application/sdp", strings.NewReader(sdpOffer))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	rec = proxyDo(h, "POST", "/live/front", hdr, "application/sdp", strings.NewReader(sdpOffer))
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/live/front/sess1" {
		t.Fatalf("status = %d, Location = %q", rec.Code, rec.Header().Get("Location"))
	}
}
