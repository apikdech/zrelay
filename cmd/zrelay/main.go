// Command zrelay is the relay daemon and its tools (relay-spec §6).
//
// Exit codes: 0 OK, 1 the check failed (memory budget over 80% of
// max_memory_mb) or the daemon stopped with an error, 2 usage error or
// invalid configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/pkg/secretref"
)

const usage = `usage: zrelay [--env-file <path>] <command> [flags]

commands:
  run --config <path>            run the daemon (SIGTERM or SIGINT stops it within shutdown_grace_seconds)
  config check --config <path>   validate the config and print the memory budget and key fingerprint
  onvif discover --config <path> --camera <id> [--listen 60s]
                                 list the camera's ONVIF event topics, stream its
                                 notifications and suggest rules
  key generate [--qr]            print a new master key (see "zrelay key")
  key fingerprint --key <ref>
  key api-token-hash --key <ref>
`

const (
	exitOK      = 0
	exitFailed  = 1
	exitInvalid = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the global flags and dispatches to a subcommand.
func run(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("zrelay", stderr)
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if code, ok := loadEnvFile(*envFile, stderr); !ok {
		return code
	}
	rest := fs.Args()
	switch {
	case len(rest) >= 1 && rest[0] == "run":
		return runDaemon(rest[1:], stderr)
	case len(rest) >= 2 && rest[0] == "config" && rest[1] == "check":
		return configCheck(rest[2:], stdout, stderr)
	case len(rest) >= 2 && rest[0] == "onvif" && rest[1] == "discover":
		return onvifDiscover(rest[2:], stdout, stderr)
	case len(rest) >= 1 && rest[0] == "key":
		return keyCmd(rest[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, usage)
		return exitInvalid
	}
}

// version is reported in sidecars and the status object; releases set it
// with -ldflags "-X main.version=...".
var version = "dev"

// runApp is app.Run; tests replace it.
var runApp = app.Run

// runDaemon implements "zrelay run" (relay-spec §6): load the config and run
// the relay until SIGTERM or SIGINT, then shut down within
// shutdown_grace_seconds.
func runDaemon(args []string, stderr io.Writer) int {
	fs := newFlagSet("zrelay run", stderr)
	path := fs.String("config", "", "config file `path`")
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: zrelay run --config <path>")
		return exitInvalid
	}
	if code, ok := loadEnvFile(*envFile, stderr); !ok {
		return code
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: run: %v\n", err)
		return exitInvalid
	}
	if r := config.Budget(cfg); !r.OK {
		slog.Warn("memory budget exceeds 80% of max_memory_mb; run \"zrelay config check\"",
			"total_bytes", r.Total, "limit_bytes", r.Limit)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runApp(ctx, cfg, app.Options{Version: version}); err != nil {
		fmt.Fprintf(stderr, "zrelay: run: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// configCheck implements "zrelay config check" (relay-spec §5.3, design
// §8.1): load and validate the config, print the budget table, and fail
// when the budget is over 80% of max_memory_mb.
func configCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("zrelay config check", stderr)
	path := fs.String("config", "", "config file `path`")
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: zrelay config check --config <path>")
		return exitInvalid
	}
	if code, ok := loadEnvFile(*envFile, stderr); !ok {
		return code
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: config check: %v\n", err)
		return exitInvalid
	}
	fp, err := config.KeyFingerprint(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: config check: encryption.master_key: %v\n", err)
		return exitInvalid
	}
	r := config.Budget(cfg)
	r.WriteTable(stdout)
	fmt.Fprintf(stdout, "key fingerprint: %s\n", fp)
	if !r.OK {
		fmt.Fprintf(stderr, "zrelay: config check: memory budget exceeds 80%% of max_memory_mb (total %d bytes > limit %d bytes)\n", r.Total, r.Limit)
		return exitFailed
	}
	return exitOK
}

// defaultListen is how long "onvif discover" listens without --listen
// (relay-spec §6).
const defaultListen = 60 * time.Second

// onvifDiscover implements "zrelay onvif discover" (relay-spec §4.4, §6):
// print the camera's event topics, stream its notifications for --listen
// and suggest rules. Stop early with Ctrl-C.
func onvifDiscover(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("zrelay onvif discover", stderr)
	path := fs.String("config", "", "config file `path`")
	camID := fs.String("camera", "", "camera `id` from the config")
	listen := fs.Duration("listen", defaultListen, "how long to stream notifications")
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *path == "" || *camID == "" || *listen <= 0 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: zrelay onvif discover --config <path> --camera <id> [--listen 60s]")
		return exitInvalid
	}
	if code, ok := loadEnvFile(*envFile, stderr); !ok {
		return code
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: onvif discover: %v\n", err)
		return exitInvalid
	}
	i := slices.IndexFunc(cfg.Cameras, func(c config.Camera) bool { return c.ID == *camID })
	if i < 0 {
		fmt.Fprintf(stderr, "zrelay: onvif discover: no camera %q in %s\n", *camID, *path)
		return exitInvalid
	}
	cam := cfg.Cameras[i]
	rules, err := events.RulesFor(cam.Brand, cam.Rules)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: onvif discover: camera %q: %v\n", cam.ID, err)
		return exitInvalid
	}
	pp := &events.PullPoint{
		Camera:      cam.ID,
		Host:        cam.Host,
		Port:        cam.EventsPort,
		ServicePath: cam.Events.ServicePath,
		Username:    cam.Username,
		Password:    cam.Password,
		Rules:       rules,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := events.Discover(ctx, pp, *listen, stdout); err != nil {
		fmt.Fprintf(stderr, "zrelay: onvif discover: %v\n", err)
		return exitFailed
	}
	return exitOK
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse parses args into fs. It returns ok false with the exit code when
// the caller should stop: 0 for -h, 2 for a flag error.
func parse(fs *flag.FlagSet, args []string) (code int, ok bool) {
	switch err := fs.Parse(args); {
	case err == nil:
		return 0, true
	case errors.Is(err, flag.ErrHelp):
		return exitOK, false
	default:
		return exitInvalid, false
	}
}

// loadEnvFile loads path when it is set (relay-spec §6 --env-file).
func loadEnvFile(path string, stderr io.Writer) (code int, ok bool) {
	if path == "" {
		return 0, true
	}
	if err := secretref.LoadEnvFile(path); err != nil {
		fmt.Fprintf(stderr, "zrelay: %v\n", err)
		return exitInvalid, false
	}
	return 0, true
}
