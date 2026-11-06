package deploy_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

type statement struct {
	Effect    string          `json:"Effect"`
	Action    []string        `json:"Action"`
	Resource  []string        `json:"Resource"`
	Condition json.RawMessage `json:"Condition"`
}

type policy struct {
	Version   string      `json:"Version"`
	Statement []statement `json:"Statement"`
}

func loadPolicy(t *testing.T, name string) policy {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var p policy
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

// allowed returns the set of actions the policy allows.
func allowed(p policy) []string {
	var out []string
	for _, s := range p.Statement {
		if s.Effect == "Allow" {
			out = append(out, s.Action...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// The relay policy (design §6.5): the listed actions on <root>/*, and
// DeleteObject only on dot-named staging objects.
func TestRelayPolicyShape(t *testing.T) {
	p := loadPolicy(t, "minio-relay-policy.json")
	want := []string{
		"s3:AbortMultipartUpload", "s3:DeleteObject", "s3:GetObject", "s3:ListBucket",
		"s3:ListBucketMultipartUploads", "s3:ListMultipartUploadParts", "s3:PutObject",
	}
	if got := allowed(p); !slices.Equal(got, want) {
		t.Errorf("relay policy actions = %v, want %v", got, want)
	}
	for _, s := range p.Statement {
		if s.Effect != "Allow" {
			t.Errorf("relay policy has a %s statement; only Allow is expected", s.Effect)
		}
		for _, a := range s.Action {
			if strings.Contains(a, "*") {
				t.Errorf("wildcard action %q", a)
			}
		}
		if !slices.Contains(s.Action, "s3:DeleteObject") {
			continue
		}
		if len(s.Action) != 1 {
			t.Errorf("DeleteObject shares a statement with %v", s.Action)
		}
		if len(s.Resource) != 1 || !strings.HasSuffix(s.Resource[0], "/zrelay/clips/*/.*") {
			t.Errorf("DeleteObject resources = %v, want only <bucket>/zrelay/clips/*/.*", s.Resource)
		}
	}
}

func TestZviewPolicyIsReadOnly(t *testing.T) {
	p := loadPolicy(t, "minio-zview-policy.json")
	want := []string{"s3:GetObject", "s3:ListBucket"}
	if got := allowed(p); !slices.Equal(got, want) {
		t.Errorf("zview policy actions = %v, want %v", got, want)
	}
}
