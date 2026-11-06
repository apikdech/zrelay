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
// until interrupted), --env-file. Exit codes: 0 OK, 2 usage or config error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
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
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *path == "" || *camID == "" || *interval <= 0 || *duration < 0 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: camprobe --config <path> --camera <id> [--interval 5m] [--duration 0] [--env-file <path>]")
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
