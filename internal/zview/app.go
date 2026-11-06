package zview

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// Content-Security-Policy for the single origin (ui-spec §2.4 with design
// §7.1: connect-src 'self'). Sent as Report-Only until Task 42 enforces it.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' blob: data:; " +
	"media-src 'self' blob:; " +
	"worker-src 'self'; " +
	"connect-src 'self'; " +
	"manifest-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// withSecurityHeaders sets the ui-spec §2.4 headers on every response.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy-Report-Only", contentSecurityPolicy)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

// assetsDir holds the content-hashed build output (Vite's default).
const assetsDir = "assets/"

// appHandler serves the built SPA from fsys. A path that names no file and
// has no extension is a client-side route and gets index.html. index.html,
// sw.js and every other un-hashed file are revalidated on each load; hashed
// assets are immutable.
type appHandler struct {
	fsys fs.FS // nil: no app configured
}

func newAppHandler(dir string) *appHandler {
	if dir == "" {
		return &appHandler{}
	}
	return &appHandler{fsys: os.DirFS(dir)}
}

func (a *appHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeProblem(w, r, http.StatusMethodNotAllowed, "")
		return
	}
	if a.fsys == nil {
		writeProblem(w, r, http.StatusNotFound, "")
		return
	}
	// Clean against a virtual root so ".." can never climb out of the app dir.
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	fi, err := fs.Stat(a.fsys, name)
	if err != nil || fi.IsDir() {
		if path.Ext(name) != "" || strings.HasPrefix(name, assetsDir) {
			writeProblem(w, r, http.StatusNotFound, "")
			return
		}
		name = "index.html"
		if fi, err = fs.Stat(a.fsys, name); err != nil {
			writeProblem(w, r, http.StatusNotFound, "")
			return
		}
	}
	f, err := a.fsys.Open(name)
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, "")
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		writeProblem(w, r, http.StatusInternalServerError, "")
		return
	}
	if strings.HasPrefix(name, assetsDir) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, fi.ModTime(), rs)
}
