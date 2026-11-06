//go:build unix

package local

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/storagetest"
)

func newDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(t.TempDir(), "cctv")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestLocalSuite(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Provider { return newDriver(t) })
}

func TestLocalRecovererSuite(t *testing.T) {
	storagetest.RunRecoverer(t,
		func(t *testing.T) (storage.Provider, storage.Recoverer) {
			d := newDriver(t)
			return d, d
		},
		func(t *testing.T, w storage.ObjectWriter) {
			// kill -9: the kernel closes the descriptor; nothing is renamed or removed.
			if err := w.(*objectWriter).f.Close(); err != nil {
				t.Fatalf("close abandoned writer: %v", err)
			}
		})
}

func TestLocalNoTmpFilesAfterPutSmall(t *testing.T) {
	base := t.TempDir()
	d, err := New(base, "cctv")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()
	for _, k := range []string{
		"status/relay.json",
		"status/relay.json", // overwrite
		"clips/cam/2026/11/06/.145959123_01JC0000000000000000000000.json.part",
		"clips/cam/2026/11/06/145959123_01JC0000000000000000000000.json",
	} {
		if err := d.PutSmall(ctx, k, []byte(`{"k":"`+k+`"}`), "application/json"); err != nil {
			t.Fatalf("PutSmall(%q): %v", k, err)
		}
	}
	// An oversized write must not leave a temporary behind either.
	_ = d.PutSmall(ctx, "status/big.json", make([]byte, 1<<20+1), "application/json")

	err = filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary left behind: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalLayout(t *testing.T) {
	base := t.TempDir()
	d, err := New(base, "cctv")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()
	if err := d.PutSmall(ctx, "status/relay.json", []byte("{}"), "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "cctv", "status", "relay.json")); err != nil {
		t.Errorf("PutSmall did not land at <base>/<root>/<key>: %v", err)
	}

	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000000.zrec"
	w, err := d.Create(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "cctv", "clips", "cam", "2026", "11", "06")
	staging := filepath.Join(dir, ".145959123_01JC0000000000000000000000.zrec.part")
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("streaming write is not staged as .<name>.part: %v", err)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging file still present after Commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "145959123_01JC0000000000000000000000.zrec")); err != nil {
		t.Errorf("committed object missing: %v", err)
	}

	incs, err := d.ListIncomplete(ctx, "", 0)
	if err != nil || len(incs) != 0 {
		t.Errorf("ListIncomplete after Commit = %v, %v; want none", incs, err)
	}
}

func TestLocalIncompleteFields(t *testing.T) {
	d := newDriver(t)
	ctx := t.Context()
	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000000.zrec"
	w, err := d.Create(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	_ = w.(*objectWriter).f.Close()

	incs, err := d.ListIncomplete(ctx, "clips", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 1 {
		t.Fatalf("ListIncomplete = %+v, want one", incs)
	}
	inc := incs[0]
	want := "clips/cam/2026/11/06/.145959123_01JC0000000000000000000000.zrec.part"
	if inc.Key != key || inc.StagingKey != want || inc.UploadID != "" || inc.Size != 4096 {
		t.Errorf("Incomplete = %+v, want Key %q StagingKey %q Size 4096", inc, key, want)
	}
}

func TestLocalETagUsesInode(t *testing.T) {
	d := newDriver(t)
	ctx := t.Context()
	const key = "status/relay.json"
	if err := d.PutSmall(ctx, key, []byte("aaaa"), "application/json"); err != nil {
		t.Fatal(err)
	}
	a, err := d.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutSmall(ctx, key, []byte("bbbb"), "application/json"); err != nil {
		t.Fatal(err)
	}
	// Simulate a coarse-mtime mount: same size and same mtime as before.
	p := filepath.Join(d.dir, "status", "relay.json")
	if err := os.Chtimes(p, a.ModTime, a.ModTime); err != nil {
		t.Fatal(err)
	}
	b, err := d.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !b.ModTime.Equal(a.ModTime) || b.Size != a.Size {
		t.Fatalf("setup: size/mtime differ (%v %d vs %v %d)", a.ModTime, a.Size, b.ModTime, b.Size)
	}
	if a.ETag == b.ETag {
		t.Errorf("ETag %q repeated across an overwrite with equal size and mtime", a.ETag)
	}
}

func TestLocalNew(t *testing.T) {
	base := t.TempDir()
	if _, err := New(base, ""); err != nil {
		t.Errorf("New(base, \"\") error = %v, want nil", err)
	}
	if _, err := New(base, "a/b"); err != nil {
		t.Errorf("New(base, a/b) error = %v, want nil", err)
	}
	repeat := filepath.Join(base, "cctv")
	if err := os.Mkdir(repeat, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(repeat, "cctv"); err == nil {
		t.Error("New(<x>/cctv, cctv) error = nil, want root-repeat error (Contract §7.1)")
	}
	for _, root := range []string{"../x", "/abs", "a//b", "a\\b"} {
		if _, err := New(base, root); err == nil {
			t.Errorf("New(base, %q) error = nil, want error", root)
		}
	}
	if _, err := New("", "cctv"); err == nil {
		t.Error("New(\"\", cctv) error = nil, want error")
	}
	if _, err := New(filepath.Join(base, "missing"), "cctv"); err == nil {
		t.Error("New(missing base_dir) error = nil, want error")
	}
	if d, err := New(base, "cctv"); err != nil || d.Name() != "local" {
		t.Errorf("New(base, cctv) = %v, %v; want Name local", d, err)
	}
}

func TestLocalModTimeUTC(t *testing.T) {
	d := newDriver(t)
	if err := d.PutSmall(t.Context(), "a", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	info, err := d.Stat(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime.Location() != time.UTC {
		t.Errorf("ModTime location = %v, want UTC", info.ModTime.Location())
	}
}

func TestLocalIncompleteIgnoresDegenerateNames(t *testing.T) {
	d := newDriver(t)
	for _, k := range []string{"clips/.part", "clips/..part", "clips/...part", "clips/....part", "clips/x.part"} {
		if err := d.PutSmall(t.Context(), k, []byte("x"), ""); err != nil {
			t.Fatalf("PutSmall(%q): %v", k, err)
		}
	}
	incs, err := d.ListIncomplete(t.Context(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 0 {
		t.Errorf("ListIncomplete = %+v, want none", incs)
	}
}

// crashLocal creates key, writes data and abandons the writer as kill -9 would.
func crashLocal(t *testing.T, d *Driver, key string, data []byte) {
	t.Helper()
	w, err := d.Create(t.Context(), key)
	if err != nil {
		t.Fatalf("Create(%q): %v", key, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.(*objectWriter).f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalIncompletePrefixNamesFinalKey(t *testing.T) {
	d := newDriver(t)
	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000000.zrec"
	const staging = "clips/cam/2026/11/06/.145959123_01JC0000000000000000000000.zrec.part"
	crashLocal(t, d, key, []byte("abc"))

	incs, err := d.ListIncomplete(t.Context(), key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 1 {
		t.Fatalf("ListIncomplete(%q) = %+v, want exactly one", key, incs)
	}
	if incs[0].Key != key || incs[0].StagingKey != staging || incs[0].Size != 3 {
		t.Errorf("Incomplete = %+v, want Key %q StagingKey %q Size 3", incs[0], key, staging)
	}
}

func TestLocalRecoverCommitRejectsMismatchedStaging(t *testing.T) {
	d := newDriver(t)
	ctx := t.Context()
	const a = "clips/cam/2026/11/06/145959123_01JC0000000000000000000000.zrec"
	const aStaging = "clips/cam/2026/11/06/.145959123_01JC0000000000000000000000.zrec.part"
	const b = "clips/cam/2026/11/06/150000000_01JC0000000000000000000001.zrec"
	crashLocal(t, d, a, []byte("abc"))
	if err := d.PutSmall(ctx, "clips/...part", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}

	for _, inc := range []storage.Incomplete{
		{Key: b, StagingKey: aStaging},                   // stages a, not b
		{Key: "status/relay.json", StagingKey: aStaging}, // any file onto any key
		{Key: "clips/..", StagingKey: "clips/...part"},   // degenerate staging name
		{Key: "clips/.", StagingKey: "clips/...part"},
		{Key: "clips/x", StagingKey: "clips/x"}, // not a staging name at all
		{Key: "../x", StagingKey: "../.x.part"}, // escapes the root
	} {
		if _, err := d.RecoverCommit(ctx, inc); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("RecoverCommit(%+v) error = %v, want ErrInvalidKey", inc, err)
		}
	}

	// Nothing was renamed: both staged files are intact and no target exists.
	for _, k := range []string{aStaging, "clips/...part"} {
		if _, err := d.Stat(ctx, k); err != nil {
			t.Errorf("Stat(%q) after rejected RecoverCommit: %v", k, err)
		}
	}
	for _, k := range []string{a, b, "status/relay.json"} {
		if _, err := d.Stat(ctx, k); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("Stat(%q) error = %v, want ErrNotFound", k, err)
		}
	}
}
