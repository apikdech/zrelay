package deploy_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The relay reports main.version in every sidecar (recorder.version) and in
// status/relay.json (Contract §6.2, §7.5.1). The production image must set it
// at build time, otherwise every clip says "dev".
func TestDockerfileStampsRelayVersion(t *testing.T) {
	df := readFile(t, "Dockerfile.zrelay")
	if !regexp.MustCompile(`(?m)^ARG VERSION(=\S*)?\s*$`).MatchString(df) {
		t.Error("Dockerfile.zrelay does not declare ARG VERSION")
	}
	build := regexp.MustCompile(`(?m)^RUN .*go build.*$`).FindString(df)
	if build == "" {
		t.Fatal("no go build line in Dockerfile.zrelay")
	}
	if !strings.Contains(build, "-X main.version=${VERSION}") {
		t.Errorf("go build does not stamp main.version from VERSION: %s", build)
	}
	// ARG must be declared in the build stage, before the go build line.
	if i, j := strings.Index(df, "ARG VERSION"), strings.Index(df, "go build"); i < 0 || i > j {
		t.Error("ARG VERSION must come before the go build line")
	}
}

func TestComposePassesVersionToRelayBuild(t *testing.T) {
	c := readFile(t, "compose.yaml")
	if !regexp.MustCompile(`(?m)^\s+VERSION: \$\{ZRELAY_VERSION:-dev\}\s*$`).MatchString(c) {
		t.Error("compose.yaml does not pass build arg VERSION from ZRELAY_VERSION")
	}
}

func TestDeployScriptSetsVersionFromGit(t *testing.T) {
	s := readFile(t, "deploy.sh")
	if !strings.Contains(s, "git describe") {
		t.Error("deploy.sh does not derive the version with git describe")
	}
	if !regexp.MustCompile(`(?m)^export ZRELAY_VERSION\b`).MatchString(s) {
		t.Error("deploy.sh does not export ZRELAY_VERSION for compose")
	}
}
