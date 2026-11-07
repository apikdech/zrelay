package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
)

// TestRunLogsStructuredJSONToStdout: relay-spec §4.12 and design §8.3 say
// logs are structured JSON (log/slog) on stdout, and only there. Library code
// that logs through the standard log package joins the same stream.
func TestRunLogsStructuredJSONToStdout(t *testing.T) {
	setExampleEnv(t)
	cfgPath := exampleConfigFile(t, nil)
	oldLogger, oldOut, oldFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	})
	old := runApp
	runApp = func(context.Context, *config.Config, app.Options) error {
		slog.Info("camera state", "camera", "cam-1", "state", "ready")
		log.Printf("library says %s", "hello")
		return nil
	}
	t.Cleanup(func() { runApp = old })

	code, out, errOut := runCLI("run", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.TrimSpace(errOut) != "" {
		t.Errorf("stderr is not empty; logs go to stdout only:\n%s", errOut)
	}
	var sawSlog, sawLib bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stdout line is not a JSON object: %q (%v)", line, err)
		}
		switch rec["msg"] {
		case "camera state":
			sawSlog = rec["camera"] == "cam-1" && rec["state"] == "ready"
		case "library says hello":
			sawLib = true
		}
	}
	if !sawSlog {
		t.Errorf("the slog record with its fields is not in stdout:\n%s", out)
	}
	if !sawLib {
		t.Errorf("the standard log line is not in stdout as a JSON record:\n%s", out)
	}
}
