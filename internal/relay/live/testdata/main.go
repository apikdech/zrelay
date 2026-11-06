// Command testdata serves whep.html on the same origin as /live/*, which it
// reverse-proxies to a zrelay (or camprobe --live) signaling listener, so a
// phone can try WHEP without the zview-server proxy. Test-only, not shipped:
//
//	go run ./internal/relay/live/testdata --listen <tailscale-ip>:8080 --upstream http://127.0.0.1:9465
//
// then open http://<tailscale-ip>:8080/ on the phone (plain HTTP over the
// tailnet; WebRTC on iOS and Android needs no secure context to receive).
// The bearer token is typed into the page and only forwarded, never logged.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

//go:embed whep.html
var page []byte

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen `address`")
	upstream := flag.String("upstream", "http://127.0.0.1:9465", "signaling base `URL` that /live/ is proxied to (camprobe's \"live listening addr\")")
	flag.Parse()
	target, err := url.Parse(*upstream)
	if err != nil || target.Host == "" {
		fmt.Fprintf(os.Stderr, "testdata: invalid --upstream %q\n", *upstream)
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.Handle("/live/", httputil.NewSingleHostReverseProxy(target))
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving whep.html on http://%s/, proxying /live/ to %s", ln.Addr(), target.Host)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}
