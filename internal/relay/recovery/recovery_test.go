package recovery_test

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/recovery"
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

func TestRunRejectsBadConfig(t *testing.T) {
	d, err := local.New(t.TempDir(), "cctv")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cfg  recovery.Config
	}{
		{"NilProvider", recovery.Config{Recoverer: d, Cameras: []string{"front"}, Now: time.Now()}},
		{"NilRecoverer", recovery.Config{Provider: d, Cameras: []string{"front"}, Now: time.Now()}},
		{"ZeroNow", recovery.Config{Provider: d, Recoverer: d, Cameras: []string{"front"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := recovery.Run(t.Context(), tc.cfg)
			if err == nil {
				t.Fatal("Run succeeded, want an error")
			}
			if rep != (recovery.Report{}) {
				t.Errorf("Report = %+v, want zero", rep)
			}
		})
	}
}

// TestDefaultOlderThan pins Config.OlderThan == 0 to the 2 minute default of
// Contract §7.4: a 1 minute old candidate is left alone, a 3 minute old one is
// recovered.
func TestDefaultOlderThan(t *testing.T) {
	base := t.TempDir()
	d, err := local.New(base, "cctv")
	if err != nil {
		t.Fatal(err)
	}
	e := env{p: d, r: d, pad: 4096, crash: func(*testing.T, storage.ObjectWriter) {}}
	now := time.Now().UTC()
	young := newClip(t, camera, now.Add(-time.Hour), 20)
	old := newClip(t, camera, now.Add(-time.Hour+time.Second), 21)
	for _, c := range []testClip{young, old} {
		e.putStaging(t, c, camera)
		e.crashVideo(t, c, camera, false)
	}
	setAge := func(c testClip, age time.Duration) {
		mt := now.Add(-age)
		for _, p := range []string{c.keys.StagingSidecar, partName(c.keys.Video)} {
			if err := os.Chtimes(filepath.Join(base, "cctv", filepath.FromSlash(p)), mt, mt); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
		}
	}
	setAge(young, time.Minute)
	setAge(old, 3*time.Minute)

	rep, err := recovery.Run(t.Context(), recovery.Config{
		Provider: d, Recoverer: d, Cameras: []string{camera}, Now: now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReport(t, rep, recovery.Report{Recovered: 1})
	if e.exists(t, young.keys.Sidecar) || !e.exists(t, old.keys.Sidecar) {
		t.Error("the default age did not separate the 1 minute and 3 minute candidates")
	}
	if got, want := e.staged(t), []string{young.keys.StagingSidecar}; !slices.Equal(got, want) {
		t.Errorf("staging = %q, want %q", got, want)
	}
}
