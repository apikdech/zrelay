// Command livefixture is a test-only helper, not shipped. It runs the real
// live view server (internal/relay/live) over a camera Hub fed by the
// in-process RTSP test fixture, so a browser test can watch real WebRTC video
// through zview-server's live proxy:
//
//	go run ./internal/testutil/cmd/livefixture [--camera front] [--max-viewers 4] [--audio] [--key ENV:NAME]
//
// Every port is ephemeral on 127.0.0.1. Once listening it prints exactly one
// JSON line on stdout:
//
//	{"upstream":"http://127.0.0.1:P","camera":"front","key":"zk1-...",
//	 "api_token_sha256":"<hex>","bearer":"<base64url K_api>"}
//
// "key" is a freshly generated throwaway master key, or the one --key names
// (an ENV: reference, never a literal) so live video can share the key of
// recorded fixture clips; "api_token_sha256" is what zview-server's config
// needs to accept it. It runs until interrupted.
// Exit codes: 0 clean stop, 1 runtime failure, 2 usage error.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/bearer"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/live"
	"github.com/apikdech/zrelay-v2/internal/testutil"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const defaultFixture = "testdata/video/h264-640x360-gop2-10s.h264"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil))
}

// info is the one line printed on stdout.
type info struct {
	Upstream string `json:"upstream"`
	Camera   string `json:"camera"`
	Key      string `json:"key"`
	Hash     string `json:"api_token_sha256"`
	Bearer   string `json:"bearer"`
}

// fatal is panicked by tb.Fatal* to unwind out of the testutil helpers.
type fatal struct{ msg string }

// tb is the slice of testing.TB the testutil fixtures use, outside a test.
type tb struct {
	testing.TB // nil: any method not defined below panics
	mu         sync.Mutex
	cleanups   []func()
	stderr     io.Writer
}

func (t *tb) Helper() {}
func (t *tb) Cleanup(f func()) {
	t.mu.Lock()
	t.cleanups = append(t.cleanups, f)
	t.mu.Unlock()
}
func (t *tb) Fatal(args ...any)                 { panic(fatal{fmt.Sprint(args...)}) }
func (t *tb) Fatalf(format string, args ...any) { panic(fatal{fmt.Sprintf(format, args...)}) }
func (t *tb) Errorf(format string, args ...any) {
	fmt.Fprintf(t.stderr, "livefixture: "+format+"\n", args...)
}
func (t *tb) runCleanups() {
	t.mu.Lock()
	fs := t.cleanups
	t.cleanups = nil
	t.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

// hubSink feeds the Hub the way the camera supervisor does.
type hubSink struct{ hub *camera.Hub }

func (s hubSink) Codec(c camera.Codec, ts camera.TimeSource) { s.hub.Codec(c, ts) }
func (s hubSink) Video(au *camera.AccessUnit)                { s.hub.Video(au) }
func (s hubSink) Audio(p *camera.AudioPacket)                { s.hub.Audio(p) }

// keyFromRef resolves an ENV: reference to a master key. A literal key is
// refused (every --key flag takes a ref), and errors never echo the value.
func keyFromRef(ref string) (zrec.MasterKey, error) {
	name, ok := strings.CutPrefix(ref, "ENV:")
	if !ok || name == "" {
		return zrec.MasterKey{}, errors.New("must be an ENV:<name> reference")
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return zrec.MasterKey{}, fmt.Errorf("environment variable %s is not set", name)
	}
	m, err := zrec.ParseMasterKey(strings.TrimSpace(v))
	if err != nil {
		return zrec.MasterKey{}, fmt.Errorf("%s does not hold a valid master key", name)
	}
	return m, nil
}

// run serves until ctx is cancelled. ready, when set, is called with the
// printed info once the server is listening.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(info)) (code int) {
	fs := flag.NewFlagSet("livefixture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	camID := fs.String("camera", "front", "camera `id`")
	maxViewers := fs.Int("max-viewers", 4, "live.max_viewers")
	withAudio := fs.Bool("audio", false, "also send G.711 audio")
	keyRef := fs.String("key", "", "master key `ref` (ENV:NAME), default a fresh throwaway key")
	fixture := fs.String("fixture", defaultFixture, "Annex-B H.264 `file` the fake camera replays")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *maxViewers < 1 {
		fmt.Fprintln(stderr, "usage: livefixture [--key ENV:<name>] [--camera <id>] [--max-viewers N] [--audio] [--fixture <file>]")
		return 2
	}
	if _, err := os.Stat(*fixture); err != nil {
		fmt.Fprintf(stderr, "livefixture: %v\n", err)
		return 2
	}

	t := &tb{stderr: stderr}
	defer t.runCleanups()
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(fatal)
			if !ok {
				panic(r)
			}
			fmt.Fprintf(stderr, "livefixture: %s\n", f.msg)
			code = 1
		}
	}()

	var master zrec.MasterKey
	if *keyRef != "" {
		m, err := keyFromRef(*keyRef)
		if err != nil {
			fmt.Fprintf(stderr, "livefixture: --key: %v\n", err)
			return 2
		}
		master = m
	} else {
		m, err := zrec.GenerateMasterKey(rand.Reader)
		if err != nil {
			fmt.Fprintf(stderr, "livefixture: %v\n", err)
			return 1
		}
		master = m
	}
	keys := zrec.Derive(master)
	sum := zrec.APITokenHash(keys)
	verifier, err := bearer.New([]string{hex.EncodeToString(sum[:])})
	if err != nil {
		fmt.Fprintf(stderr, "livefixture: %v\n", err)
		return 1
	}

	rtsp := testutil.StartRTSPServer(t, testutil.RTSPServerOpts{
		AUs: testutil.LoadAnnexB(t, *fixture), Loop: true, Audio: *withAudio,
	})
	hub := camera.NewHub(*camID)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			_ = camera.RunRTSP(ctx, rtsp.URL(), hubSink{hub})
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	srv, err := live.NewServer(live.Config{
		Listen: "127.0.0.1:0", AdvertiseIP: "127.0.0.1", ICEUDPPort: 0,
		MaxViewers: *maxViewers, Verifier: verifier, Hubs: map[string]*camera.Hub{*camID: hub},
	})
	if err != nil {
		fmt.Fprintf(stderr, "livefixture: %v\n", err)
		return 1
	}
	defer srv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(stderr, "livefixture: %v\n", err)
		return 1
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	i := info{
		Upstream: "http://" + ln.Addr().String(), Camera: *camID, Key: master.String(),
		Hash: hex.EncodeToString(sum[:]), Bearer: zrec.BearerToken(keys),
	}
	line, _ := json.Marshal(i)
	fmt.Fprintf(stdout, "%s\n", line)
	if ready != nil {
		ready(i)
	}
	<-ctx.Done()
	return 0
}
