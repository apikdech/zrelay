package deploy_test

import (
	"os"
	"strings"
	"testing"
)

// runbookCodeLines returns the lines inside fenced code blocks of the runbook.
func runbookCodeLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			in = !in
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	return out
}

// The master key must never be typed into a shell command: it would be
// recorded in shell history (relay-spec §6, §5.1).
func TestRunbookNeverPutsMasterKeyOnACommandLine(t *testing.T) {
	var sawMasterWrite bool
	for _, l := range runbookCodeLines(t) {
		if strings.Contains(l, "zk1-") {
			t.Errorf("runbook command contains a literal key: %q", l)
		}
		if strings.Contains(l, "> deploy/secrets/zrelay_master") {
			sawMasterWrite = true
		}
	}
	if !sawMasterWrite {
		t.Fatal("runbook no longer shows how to write deploy/secrets/zrelay_master")
	}
}

func TestRunbookReadsMasterKeyWithoutEcho(t *testing.T) {
	var read, unset bool
	for _, l := range runbookCodeLines(t) {
		if strings.Contains(l, "read -rs") {
			read = true
		}
		if strings.Contains(l, "unset") {
			unset = true
		}
	}
	if !read || !unset {
		t.Fatalf("master key must be read silently and unset afterwards (read=%v unset=%v)", read, unset)
	}
}
