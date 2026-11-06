package zview

import (
	"fmt"
	"net/http"

	"github.com/apikdech/zrelay-v2/internal/bearer"
	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

type server struct {
	idx   *Index
	store storage.ReadProvider
}

// NewServer returns the zview-server handler (design §7.1, ui-spec §3.4
// minus video-link):
//
//	GET /healthz                       liveness, no auth
//	GET /api/v1/{status,cameras,clips,clips/{clip_id},clips/{clip_id}/video,coverage}
//	/live/*                            live, behind the same bearer check
//	everything else                    the SPA from cfg.Server.AppDir
//
// Everything under /api/ and /live/ requires the bearer credential. cfg must
// have passed LoadConfig; NewServer panics on token hashes bearer.New
// rejects. A nil live leaves /live/ answering 404.
func NewServer(cfg *Config, idx *Index, r storage.ReadProvider, live http.Handler) http.Handler {
	v, err := bearer.New(cfg.Server.APITokenSHA256)
	if err != nil {
		panic(fmt.Sprintf("zview: NewServer: %v", err))
	}
	s := &server{idx: idx, store: r}
	lim := newFailureLimiter(clock.Real, authFailLimit, authFailWindow)
	authed := func(h http.Handler) http.Handler { return authenticated(v, lim, h) }

	mux := http.NewServeMux()
	mux.Handle("/healthz", getOnly(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("ok\n"))
		}
	}))
	mux.Handle("/api/", authed(getOnly(s.notFound)))
	mux.Handle("/api/v1/status", authed(getOnly(s.status)))
	mux.Handle("/api/v1/cameras", authed(getOnly(s.cameras)))
	mux.Handle("/api/v1/clips", authed(getOnly(s.clips)))
	mux.Handle("/api/v1/clips/{clip_id}", authed(getOnly(s.clip)))
	mux.Handle("/api/v1/clips/{clip_id}/video", authed(getOnly(s.video)))
	mux.Handle("/api/v1/coverage", authed(getOnly(s.coverage)))
	if live != nil {
		mux.Handle("/live/", authed(live))
	} else {
		mux.Handle("/live/", authed(http.HandlerFunc(s.notFound)))
	}
	mux.Handle("/", newAppHandler(cfg.Server.AppDir))
	return withSecurityHeaders(mux)
}
