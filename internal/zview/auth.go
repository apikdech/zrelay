package zview

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/apikdech/zrelay-v2/internal/bearer"
)

// authenticated wraps next so it runs only for a valid bearer credential
// (ui-spec §3.2). Every rejection has the same 401 body, so a client learns
// nothing about why its credential failed. Failures are counted per client
// (clientKey); once one client has exceeded authFailLimit failures in a window its
// further failures get 429. A valid credential is never refused, so a
// misbehaving client behind the same proxy address cannot lock the family out.
func authenticated(v *bearer.Verifier, lim *failureLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v.Check(r.Header.Get("Authorization")) {
			next.ServeHTTP(w, r)
			return
		}
		if !lim.fail(clientKey(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(int(authFailWindow.Seconds())))
			writeProblem(w, r, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, r, http.StatusUnauthorized, "")
	})
}

// tailscaleUserHeader is set by tailscale serve to the login name of the
// tailnet user whose request it is proxying.
const tailscaleUserHeader = "Tailscale-User-Login"

// clientKey is what failures are counted against. Behind tailscale serve
// (and a Docker port mapping) every client arrives from the same proxy
// address, so the peer alone would make the limit global: one stale device
// would turn every other device's 401 into 429. When the peer looks like that
// proxy (loopback or private) and names its user, the user is the key; any
// other request is keyed by its peer address. Other forwarding headers are
// not trusted, and neither is this one from a public peer: any client could
// send it.
func clientKey(r *http.Request) string {
	ip := clientIP(r)
	if user := r.Header.Get(tailscaleUserHeader); user != "" && proxyPeer(ip) {
		return "user:" + user
	}
	return ip
}

// proxyPeer reports whether ip is an address a local reverse proxy connects
// from: loopback, or a private/link-local one (the Docker bridge gateway).
func proxyPeer(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
}

// clientIP is the peer address.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
