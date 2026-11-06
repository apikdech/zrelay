package storage_test

import (
	"errors"
	"testing"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

func TestCleanKey(t *testing.T) {
	accept := []string{
		"clips/a/.x.part",
		"clips/front/2026/11/06/145959123_01JC0000000000000000000000.zrec",
		"status/relay.json",
		".x.json.part",
		"clips/a/..x.json.part.tmp",
		"a",
	}
	for _, k := range accept {
		got, err := storage.CleanKey(k)
		if err != nil {
			t.Errorf("CleanKey(%q) error = %v, want nil", k, err)
			continue
		}
		if got != k {
			t.Errorf("CleanKey(%q) = %q, want unchanged", k, got)
		}
	}

	reject := []string{
		"",
		"..",
		"../x",
		"a/..",
		"a/../b",
		"/abs",
		"a\\b",
		"a//b",
		"a/",
		".",
		"a/./b",
		"a\x00b",
		"a\nb",
		"a\x1fb",
		"a\x7fb",
		"a\u0085b",
		"a\xffb",
	}
	for _, k := range reject {
		_, err := storage.CleanKey(k)
		if !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("CleanKey(%q) error = %v, want ErrInvalidKey", k, err)
		}
	}
}

func TestJoinRoot(t *testing.T) {
	cases := []struct{ root, key, want string }{
		{"", "clips/a", "clips/a"},
		{"cctv", "clips/a", "cctv/clips/a"},
		{"a/b", "status/relay.json", "a/b/status/relay.json"},
		{"cctv", "", "cctv"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := storage.JoinRoot(c.root, c.key); got != c.want {
			t.Errorf("JoinRoot(%q, %q) = %q, want %q", c.root, c.key, got, c.want)
		}
	}
}
