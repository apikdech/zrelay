package recovery_test

import (
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/local"
)

func newLocalEnv(t *testing.T) env {
	t.Helper()
	base := t.TempDir()
	d, err := local.New(base, "cctv")
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	return env{
		p: d, r: d,
		pad:       4096,
		olderThan: 2 * time.Minute,
		// A killed process leaves its descriptor to the kernel; the test
		// leaves the writer unfinished. Nothing is renamed or removed.
		crash: func(*testing.T, storage.ObjectWriter) {},
		age: func(t *testing.T, keys ...string) {
			t.Helper()
			old := time.Now().Add(-10 * time.Minute)
			for _, k := range keys {
				// A streamed video is staged as ".<name>.part".
				for _, p := range []string{k, partName(k)} {
					full := filepath.Join(base, "cctv", filepath.FromSlash(p))
					if _, err := os.Lstat(full); err != nil {
						continue
					}
					if err := os.Chtimes(full, old, old); err != nil {
						t.Fatalf("Chtimes: %v", err)
					}
				}
			}
		},
	}
}

// partName maps "dir/name.zrec" to "dir/.name.zrec.part".
func partName(key string) string {
	dir, name := path.Split(key)
	return dir + "." + name + ".part"
}

func TestRecoveryLocal(t *testing.T) {
	runScenarios(t, newLocalEnv)
}
