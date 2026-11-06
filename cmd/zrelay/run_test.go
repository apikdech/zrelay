package main

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
)

// stubApp replaces app.Run for the test and records its inputs.
type stubApp struct {
	called chan struct{}
	cfg    *config.Config
	opts   app.Options
	ret    func(ctx context.Context) error
}

func installStub(t *testing.T, ret func(ctx context.Context) error) *stubApp {
	t.Helper()
	s := &stubApp{called: make(chan struct{}), ret: ret}
	old := runApp
	runApp = func(ctx context.Context, cfg *config.Config, opts app.Options) error {
		s.cfg, s.opts = cfg, opts
		close(s.called)
		return s.ret(ctx)
	}
	t.Cleanup(func() { runApp = old })
	return s
}

func TestRunUsageErrors(t *testing.T) {
	installStub(t, func(context.Context) error { t.Error("app must not start"); return nil })
	for _, args := range [][]string{
		{"run"},
		{"run", "--config", "/nonexistent/zrelay.yaml"},
		{"run", "--config", exampleConfig, "extra"},
	} {
		if code, _, errOut := runCLI(args...); code != 2 || errOut == "" {
			t.Errorf("run(%q) = %d with stderr %q, want 2 with a message", args, code, errOut)
		}
	}
}

func TestRunStartsAppAndStopsOnSIGTERM(t *testing.T) {
	setExampleEnv(t)
	cfgPath := exampleConfigFile(t, nil)
	s := installStub(t, func(ctx context.Context) error {
		<-ctx.Done() // returns when SIGTERM cancels the context
		return nil
	})

	done := make(chan int, 1)
	go func() {
		code, _, _ := runCLI("run", "--config", cfgPath)
		done <- code
	}()
	select {
	case <-s.called:
	case <-time.After(10 * time.Second):
		t.Fatal("app.Run was not called")
	}
	if s.cfg == nil || s.cfg.Relay.ID == "" || s.opts.Version == "" {
		t.Errorf("app.Run got cfg %v, version %q", s.cfg, s.opts.Version)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d after SIGTERM, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
}

func TestRunAppErrorExitsOneWithoutSecrets(t *testing.T) {
	setExampleEnv(t)
	cfgPath := exampleConfigFile(t, nil)
	installStub(t, func(context.Context) error { return errors.New("storage: boom") })
	code, _, errOut := runCLI("run", "--config", cfgPath)
	if code != 1 || !strings.Contains(errOut, "boom") {
		t.Fatalf("exit %d, stderr %q, want 1 naming the error", code, errOut)
	}
	for _, secret := range []string{"cam-pass", "s3-secret", "restream-pass"} {
		if strings.Contains(errOut, secret) {
			t.Errorf("stderr leaks %q", secret)
		}
	}
}

func TestRunHonoursEnvFile(t *testing.T) {
	cfgPath := exampleConfigFile(t, nil)
	var env strings.Builder
	for k, v := range exampleEnv {
		env.WriteString(k + "=" + v + "\n")
	}
	envPath := writeFile(t, t.TempDir(), "run.env", env.String())
	s := installStub(t, func(context.Context) error { return nil })
	if code, _, errOut := runCLI("--env-file", envPath, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	<-s.called
}
