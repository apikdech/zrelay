package testutil

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// FFProbeImage is the pinned image ffprobe runs from. It is never installed on
// the host (Global Constraints).
const FFProbeImage = "linuxserver/ffmpeg:version-7.1-cli"

// FFProbe runs ffprobe inside Docker with data on stdin (`-i pipe:0`) and
// returns its stdout. args go before the input, for example
// "-show_frames", "-of", "json". Nothing is written to disk.
//
// When Docker is not available the test is skipped, unless CI=true, where a
// missing Docker is a failure so the check cannot silently disappear.
func FFProbe(t testing.TB, data []byte, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		skipOrFailNoDocker(t, "docker not on PATH")
	} else if err := exec.Command("docker", "info").Run(); err != nil {
		skipOrFailNoDocker(t, "docker daemon unreachable")
	}
	full := append([]string{"run", "--rm", "-i", "--entrypoint", "ffprobe", FFProbeImage, "-v", "error"}, args...)
	full = append(full, "-i", "pipe:0")
	cmd := exec.Command("docker", full...)
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("testutil.FFProbe: %v\n%s", err, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Logf("ffprobe stderr: %s", stderr.String())
	}
	return stdout.Bytes()
}

func skipOrFailNoDocker(t testing.TB, why string) {
	t.Helper()
	if os.Getenv("CI") == "true" {
		t.Fatalf("testutil.FFProbe: %s (CI=true requires Docker)", why)
	}
	t.Skipf("testutil.FFProbe: %s", why)
}
