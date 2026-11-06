package storagetest

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// mcBinary is the MinIO client shipped inside MinIOImage.
const mcBinary = "mcli"

// mc runs the MinIO client inside the container and fails the test on a
// non-zero exit. The alias "local" points at the server itself.
func (m *MinIO) mc(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code, out, err := m.ctr.Exec(ctx, append([]string{mcBinary}, args...))
	if err != nil {
		t.Fatalf("mc %s: %v", args[0], err)
	}
	b, _ := io.ReadAll(out)
	if code != 0 {
		t.Fatalf("mc %s exited %d: %s", strings.Join(args[:min(len(args), 3)], " "), code, b)
	}
	return string(b)
}

// CreateUser registers a MinIO user with the given secret and attaches a new
// policy (the JSON document policyJSON, named policyName) to it, the way the
// runbook does with `mc admin policy create`, `mc admin user add` and
// `mc admin policy attach`. The user can then reach the server at Endpoint
// with its own credentials.
func (m *MinIO) CreateUser(t *testing.T, user, secret, policyName string, policyJSON []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	path := "/tmp/policy-" + policyName + ".json"
	if err := m.ctr.CopyToContainer(ctx, policyJSON, path, 0o644); err != nil {
		t.Fatalf("copy policy: %v", err)
	}
	m.mc(t, "alias", "set", "local", "http://127.0.0.1:9000", m.AccessKey, m.SecretKey)
	m.mc(t, "admin", "policy", "create", "local", policyName, path)
	m.mc(t, "admin", "user", "add", "local", user, secret)
	m.mc(t, "admin", "policy", "attach", "local", policyName, "--user", user)
}
