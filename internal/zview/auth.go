package zview

import (
	"net"
	"net/http"
	"strconv"

	"github.com/apikdech/zrelay-v2/internal/bearer"
)

// authenticated wraps next so it runs only for a valid bearer credential
// (ui-spec §3.2). Every rejection has the same 401 body, so a client learns
// nothing about why its credential failed. Failures are counted per client
// IP; once one IP has exceeded authFailLimit failures in a window its
// further failures get 429. A valid credential is never refused, so a
// misbehaving client behind the same proxy address cannot lock the family out.
func authenticated(v *bearer.Verifier, lim *failureLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v.Check(r.Header.Get("Authorization")) {
			next.ServeHTTP(w, r)
			return
		}
		if !lim.fail(clientIP(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(int(authFailWindow.Seconds())))
			writeProblem(w, r, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, r, http.StatusUnauthorized, "")
	})
}

// clientIP is the peer address. Forwarding headers are not trusted: any
// client could set them, and behind tailscale serve the peer is the proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
