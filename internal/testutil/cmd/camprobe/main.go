// Command camprobe is a test-only probe, not shipped: it supervises one
// camera from a zrelay config (RTSP session and ONVIF subscription, nothing
// else) and logs, every --interval, the camera state, the access-unit rate,
// the goroutine count and the process RSS. A one-hour run against a real
// camera must show flat goroutines and RSS (docs/LIVE-CHECKS.md, Task 9).
//
//	go run ./internal/testutil/cmd/camprobe --env-file .env \
//	    --config deploy/zrelay.example.yaml --camera tapo-c220
//
// Flags: --config, --camera, --interval (default 5m), --duration (default 0:
// until interrupted), --env-file, --live, --restream. --live also serves the
// WHEP endpoint (config live: block) and --restream the RTSP restream
// (config restream: block) from the same supervised camera, still with no
// recorder or storage (docs/LIVE-CHECKS.md, Task 12); each logs
// "<live|restream> listening addr=<host:port>". Exit codes: 0 OK, 2 usage or
// config error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apikdech/zrelay-v2/internal/bearer"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/live"
	"github.com/apikdech/zrelay-v2/internal/relay/restream"
	"github.com/apikdech/zrelay-v2/pkg/secretref"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// probeQueue is the recorder-class subscription queue that counts AUs.
const probeQueue = 256

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("camprobe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "config file `path`")
	camID := fs.String("camera", "", "camera `id` from the config")
	interval := fs.Duration("interval", 5*time.Minute, "how often to log")
	duration := fs.Duration("duration", 0, "stop after this long (0: until interrupted)")
	withLive := fs.Bool("live", false, "also serve WHEP live view (config live: block must enable it)")
	withRestream := fs.Bool("restream", false, "also serve the RTSP restream (config restream: block must enable it)")
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *path == "" || *camID == "" || *interval <= 0 || *duration < 0 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: camprobe --config <path> --camera <id> [--interval 5m] [--duration 0] [--env-file <path>] [--live] [--restream]")
		return 2
	}
	if *envFile != "" {
		if err := secretref.LoadEnvFile(*envFile); err != nil {
			fmt.Fprintf(stderr, "camprobe: %v\n", err)
			return 2
		}
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "camprobe: %v\n", err)
		return 2
	}
	i := slices.IndexFunc(cfg.Cameras, func(c config.Camera) bool { return c.ID == *camID })
	if i < 0 {
		fmt.Fprintf(stderr, "camprobe: no camera %q in %s\n", *camID, *path)
		return 2
	}
	if *withLive && cfg.Live.MaxViewers <= 0 {
		fmt.Fprintf(stderr, "camprobe: --live needs live.max_viewers > 0 in %s\n", *path)
		return 2
	}
	if *withRestream && cfg.Restream.Listen == "" {
		fmt.Fprintf(stderr, "camprobe: --restream needs restream.listen in %s\n", *path)
		return 2
	}
	cam := cfg.Cameras[i]
	rules, err := events.RulesFor(cam.Brand, cam.Rules)
	if err != nil {
		fmt.Fprintf(stderr, "camprobe: camera %q: %v\n", cam.ID, err)
		return 2
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, *duration)
		defer stop()
	}

	log := &logger{w: stdout}
	defer log.close()
	c := camera.Supervise(ctx, camera.SuperviseConfig{
		ID:      cam.ID,
		RTSPURL: cfg.RTSPURL(cam), // carries credentials: never logged
		Events: &events.PullPoint{
			Camera: cam.ID, Host: cam.Host, Port: cam.EventsPort, ServicePath: cam.Events.ServicePath,
			Username: cam.Username, Password: cam.Password, Rules: rules,
		},
		TriggerTimeout: cam.TriggerTimeout,
		OnState: func(s camera.Status) {
			log.printf("camera=%s state=%s rtsp=%s events=%s", cam.ID, s.State, s.RTSP, s.Events)
		},
	})

	if *withLive || *withRestream {
		stopServers, err := startServers(cfg, cam.ID, c.Hub, *withLive, *withRestream, log)
		if err != nil {
			fmt.Fprintf(stderr, "camprobe: %v\n", err)
			return 2
		}
		defer stopServers()
	}

	// Count access units through an ordinary recorder subscription.
	var aus atomic.Uint64
	sub := c.Hub.Subscribe(camera.ClassRecorder, probeQueue)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range sub.C() {
			if ev.Kind == camera.EventVideo {
				aus.Add(1)
			}
		}
	}()
	defer wg.Wait()
	defer sub.Close()

	tick := time.NewTicker(*interval)
	defer tick.Stop()
	last, lastAt := uint64(0), time.Now()
	for {
		select {
		case <-ctx.Done():
			return 0
		case now := <-tick.C:
			n := aus.Load()
			rate := float64(n-last) / now.Sub(lastAt).Seconds()
			last, lastAt = n, now
			st := c.Status()
			log.printf("camera=%s state=%s rtsp=%s events=%s au_per_s=%.1f goroutines=%d rss_kib=%d",
				cam.ID, st.State, st.RTSP, st.Events, rate, runtime.NumGoroutine(), rssKiB())
		}
	}
}

// startServers starts the live (WHEP) and restream servers over hub and logs
// where each listens. The returned stop function closes both.
func startServers(cfg *config.Config, id string, hub *camera.Hub, withLive, withRestream bool, log *logger) (func(), error) {
	hubs := map[string]*camera.Hub{id: hub}
	var stops []func()
	stopAll := func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
	// fail releases whatever already started and returns err.
	fail := func(err error) (func(), error) {
		stopAll()
		return nil, err
	}
	if withLive {
		verifier, err := bearer.New(cfg.Live.APITokenSHA256)
		if err != nil {
			return fail(err)
		}
		srv, err := live.NewServer(live.Config{
			Listen: cfg.Live.Listen, AdvertiseIP: cfg.Live.AdvertiseIP, ICEUDPPort: cfg.Live.ICEUDPPort,
			MaxViewers: cfg.Live.MaxViewers, Verifier: verifier, Hubs: hubs,
		})
		if err != nil {
			return fail(err)
		}
		stops = append(stops, func() { _ = srv.Close() })
		ln, err := net.Listen("tcp", cfg.Live.Listen)
		if err != nil {
			return fail(fmt.Errorf("live: %w", err))
		}
		httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = httpSrv.Serve(ln) }()
		stops = append(stops, func() { _ = httpSrv.Close() })
		log.printf("live listening addr=%s camera=%s", ln.Addr(), id)
	}
	if withRestream {
		srv, err := restream.New(restream.Config{
			Listen: cfg.Restream.Listen, Username: cfg.Restream.Username, Password: cfg.Restream.Password, Hubs: hubs,
		})
		if err != nil {
			return fail(err)
		}
		if err := srv.Start(); err != nil {
			return fail(fmt.Errorf("restream: %w", err))
		}
		stops = append(stops, srv.Close)
		log.printf("restream listening addr=%s camera=%s", srv.Addr(), id)
	}
	return stopAll, nil
}

// logger writes timestamped lines, one at a time, and stops once closed:
// the supervisor may report a last state change after run has returned.
type logger struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (l *logger) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	fmt.Fprintf(l.w, "time=%s "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func (l *logger) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// rssKiB returns the current resident set size of this process in KiB, or 0
// when ps cannot tell. ps works the same on Linux and macOS.
func rssKiB() int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}
