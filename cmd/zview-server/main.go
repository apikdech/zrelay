// Command zview-server serves the zview SPA and its read-only data API from
// one origin (design D18, §7.1):
//
//	zview-server --config <path> [--mode both] [--env-file <path>]
//
// Only --mode both exists; the app and data modes returned with Topology A
// are future work. Exit codes: 0 clean shutdown, 1 runtime failure, 2 usage
// error or invalid configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/zview"
	"github.com/apikdech/zrelay-v2/pkg/secretref"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/local"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
)

const (
	exitOK      = 0
	exitFailed  = 1
	exitInvalid = 2

	shutdownGrace     = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil))
}

// run serves until ctx is cancelled. ready, when set, is called with the
// bound address once the server is listening.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(addr string)) int {
	fs := flag.NewFlagSet("zview-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file `path`")
	mode := fs.String("mode", "both", "serving mode (only \"both\" exists: SPA and API on one origin)")
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalid
	}
	if *cfgPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: zview-server --config <path> [--mode both] [--env-file <path>]")
		return exitInvalid
	}
	if *mode != "both" {
		fmt.Fprintf(stderr, "zview-server: --mode %q is not supported; only \"both\" exists\n", *mode)
		return exitInvalid
	}
	if *envFile != "" {
		if err := secretref.LoadEnvFile(*envFile); err != nil {
			fmt.Fprintf(stderr, "zview-server: %v\n", err)
			return exitInvalid
		}
	}
	cfg, err := zview.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "zview-server: %v\n", err)
		return exitInvalid
	}
	store, err := openStorage(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "zview-server: %v\n", err)
		return exitInvalid
	}
	defer store.Close()

	idx := zview.NewIndex(store, cfg.Index, clock.Real)
	// The live proxy (design §7.1) is mounted by passing its handler here; a
	// nil handler leaves /live/ answering 404.
	handler := zview.NewServer(cfg, idx, store, nil)

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "zview-server: listen: %v\n", err)
		return exitFailed
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No write timeout: video responses stream for as long as the client reads.
	}
	slog.New(slog.NewTextHandler(stderr, nil)).Info("zview-server listening", "addr", ln.Addr().String(), "storage", store.Name())
	if ready != nil {
		ready(ln.Addr().String())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		fmt.Fprintf(stderr, "zview-server: %v\n", err)
		return exitFailed
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		fmt.Fprintf(stderr, "zview-server: shutdown: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// openStorage binds the configured driver as a storage.ReadProvider, so the
// server has no write methods at all.
func openStorage(ctx context.Context, cfg *zview.Config) (storage.ReadProvider, error) {
	switch cfg.Storage.Driver {
	case "s3":
		s := cfg.Storage.S3
		return s3.New(ctx, s3.Config{
			Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Root: cfg.Storage.Root,
			AccessKey: s.AccessKey, SecretKey: s.SecretKey, UsePathStyle: s.UsePathStyle,
		})
	case "local":
		return local.New(cfg.Storage.Local.BaseDir, cfg.Storage.Root)
	}
	return nil, fmt.Errorf("storage driver %q is not supported", cfg.Storage.Driver)
}
