// Package storagetest is the conformance suite every storage driver runs
// (relay-spec §4.9, Contract §7.2–§7.3). Drivers call Run from their own tests
// with a constructor that returns a fresh, empty provider per subtest.
package storagetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// Run executes the Provider conformance suite. newProvider is called once per
// subtest and must return a provider whose key space is empty.
func Run(t *testing.T, newProvider func(t *testing.T) storage.Provider) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, p storage.Provider)
	}{
		{"NameAndHealthCheck", testNameAndHealthCheck},
		{"PutStatOpenRange", testPutStatOpenRange},
		{"MissingKeyNotFound", testMissingKeyNotFound},
		{"InvalidKeysRejected", testInvalidKeysRejected},
		{"ListPrefixByWholeElement", testListPrefixByWholeElement},
		{"ListEmptyPrefixListsAll", testListEmptyPrefixListsAll},
		{"ListMissingPrefixEmpty", testListMissingPrefixEmpty},
		{"ListExactKey", testListExactKey},
		{"ListOmitsDotNames", testListOmitsDotNames},
		{"ListEarlyBreakSafe", testListEarlyBreakSafe},
		{"ListInvalidPrefix", testListInvalidPrefix},
		{"DotNamesExplicitAccess", testDotNamesExplicitAccess},
		{"Delete", testDelete},
		{"PutSmallOverwriteChangesETag", testPutSmallOverwriteChangesETag},
		{"PutSmallLimit", testPutSmallLimit},
		{"CreateVisibleOnlyAfterCommit", testCreateVisibleOnlyAfterCommit},
		{"AbortLeavesNothingVisible", testAbortLeavesNothingVisible},
		{"ResumeContinuesExactBytes", testResumeContinuesExactBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newProvider(t)
			t.Cleanup(func() { _ = p.Close() })
			c.fn(t, p)
		})
	}
}

// pattern returns n deterministic, position-dependent bytes so that shifted or
// duplicated ranges never compare equal by accident.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func put(t *testing.T, p storage.Provider, key string, data []byte) {
	t.Helper()
	if err := p.PutSmall(t.Context(), key, data, "application/octet-stream"); err != nil {
		t.Fatalf("PutSmall(%q): %v", key, err)
	}
}

func readAll(t *testing.T, r storage.Reader, key string) []byte {
	t.Helper()
	or, err := r.OpenRange(t.Context(), key, 0, -1)
	if err != nil {
		t.Fatalf("OpenRange(%q, 0, -1): %v", key, err)
	}
	defer or.Close()
	b, err := io.ReadAll(or)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return b
}

func listKeys(t *testing.T, r storage.Reader, prefix string) []string {
	t.Helper()
	var keys []string
	for info, err := range r.List(t.Context(), prefix) {
		if err != nil {
			t.Fatalf("List(%q): %v", prefix, err)
		}
		keys = append(keys, info.Key)
	}
	slices.Sort(keys)
	return keys
}

// writeChunks writes data to w in chunks of at most chunk bytes, checking that
// DurableOffset never decreases and never exceeds what was written.
func writeChunks(t *testing.T, w storage.ObjectWriter, data []byte, chunk int, written int64) int64 {
	t.Helper()
	last := w.DurableOffset()
	for len(data) > 0 {
		n := min(chunk, len(data))
		m, err := w.Write(data[:n])
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if m != n {
			t.Fatalf("Write wrote %d of %d bytes without error", m, n)
		}
		written += int64(n)
		data = data[n:]
		d := w.DurableOffset()
		if d < last {
			t.Fatalf("DurableOffset decreased from %d to %d", last, d)
		}
		if d > written {
			t.Fatalf("DurableOffset %d exceeds bytes written %d", d, written)
		}
		last = d
	}
	return written
}

func testNameAndHealthCheck(t *testing.T, p storage.Provider) {
	if p.Name() == "" {
		t.Error("Name() is empty")
	}
	if err := p.HealthCheck(t.Context()); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

func testPutStatOpenRange(t *testing.T, p storage.Provider) {
	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000000.json"
	data := pattern(1000, 1)
	before := time.Now().Add(-time.Hour)
	put(t, p, key, data)

	info, err := p.Stat(t.Context(), key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != key || info.Size != int64(len(data)) {
		t.Fatalf("Stat = {Key:%q Size:%d}, want {%q %d}", info.Key, info.Size, key, len(data))
	}
	if info.ETag == "" {
		t.Error("Stat ETag is empty")
	}
	if info.ModTime.Location() != time.UTC {
		t.Errorf("Stat ModTime location = %v, want UTC", info.ModTime.Location())
	}
	if info.ModTime.Before(before) {
		t.Errorf("Stat ModTime = %v, implausibly old", info.ModTime)
	}

	ranges := []struct {
		off, n int64
		want   []byte
	}{
		{0, -1, data},
		{0, int64(len(data)), data},
		{100, 50, data[100:150]},
		{900, -1, data[900:]},
		{999, 1, data[999:]},
		{990, 100, data[990:]}, // past the end: clamped to the object
	}
	for _, rg := range ranges {
		or, err := p.OpenRange(t.Context(), key, rg.off, rg.n)
		if err != nil {
			t.Fatalf("OpenRange(%d, %d): %v", rg.off, rg.n, err)
		}
		got, err := io.ReadAll(or)
		_ = or.Close()
		if err != nil {
			t.Fatalf("OpenRange(%d, %d) read: %v", rg.off, rg.n, err)
		}
		if !bytes.Equal(got, rg.want) {
			t.Errorf("OpenRange(%d, %d) returned %d bytes, want %d matching bytes", rg.off, rg.n, len(got), len(rg.want))
		}
		if or.Total != int64(len(data)) {
			t.Errorf("OpenRange(%d, %d) Total = %d, want %d", rg.off, rg.n, or.Total, len(data))
		}
		if or.ETag != info.ETag {
			t.Errorf("OpenRange(%d, %d) ETag = %q, want Stat ETag %q", rg.off, rg.n, or.ETag, info.ETag)
		}
	}
}

func testMissingKeyNotFound(t *testing.T, p storage.Provider) {
	const key = "clips/cam/missing.zrec"
	if _, err := p.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := p.OpenRange(t.Context(), key, 0, -1); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("OpenRange(missing) error = %v, want ErrNotFound", err)
	}
	// A key naming a "directory" of other objects is not itself an object.
	put(t, p, "clips/cam/a.json", []byte("{}"))
	if _, err := p.Stat(t.Context(), "clips/cam"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(prefix of an object) error = %v, want ErrNotFound", err)
	}
}

func testInvalidKeysRejected(t *testing.T, p storage.Provider) {
	ctx := t.Context()
	for _, key := range []string{"", "../x", "a/../b", "/abs", "a\\b", "a//b", "a\x00b"} {
		if _, err := p.Stat(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Stat(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if _, err := p.OpenRange(ctx, key, 0, -1); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("OpenRange(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if err := p.PutSmall(ctx, key, []byte("x"), "text/plain"); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("PutSmall(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if err := p.Delete(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Delete(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if w, err := p.Create(ctx, key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Create(%q) error = %v, want ErrInvalidKey", key, err)
			if w != nil {
				_ = w.Abort(ctx)
			}
		}
	}
}

func testListPrefixByWholeElement(t *testing.T, p storage.Provider) {
	for _, k := range []string{
		"clips/front/a.json",
		"clips/front/2026/11/06/b.json",
		"clips/front-door/x.json",
		"clips/frontx",
		"other/y.json",
	} {
		put(t, p, k, []byte(k))
	}
	got := listKeys(t, p, "clips/front")
	want := []string{"clips/front/2026/11/06/b.json", "clips/front/a.json"}
	if !slices.Equal(got, want) {
		t.Errorf("List(clips/front) = %q, want %q", got, want)
	}
	got = listKeys(t, p, "clips/front/2026")
	want = []string{"clips/front/2026/11/06/b.json"}
	if !slices.Equal(got, want) {
		t.Errorf("List(clips/front/2026) = %q, want %q", got, want)
	}
}

func testListEmptyPrefixListsAll(t *testing.T, p storage.Provider) {
	want := []string{"clips/a/1.json", "clips/b/2.json", "status/relay.json"}
	for _, k := range want {
		put(t, p, k, []byte("x"))
	}
	if got := listKeys(t, p, ""); !slices.Equal(got, want) {
		t.Errorf("List(\"\") = %q, want %q", got, want)
	}
	// Listed metadata matches Stat.
	for info, err := range p.List(t.Context(), "status") {
		if err != nil {
			t.Fatalf("List(status): %v", err)
		}
		st, err := p.Stat(t.Context(), info.Key)
		if err != nil {
			t.Fatalf("Stat(%q): %v", info.Key, err)
		}
		if info.Size != st.Size || info.Size != 1 {
			t.Errorf("List Size = %d, Stat Size = %d, want 1", info.Size, st.Size)
		}
		if info.ModTime.IsZero() {
			t.Error("List ModTime is zero")
		}
	}
}

func testListMissingPrefixEmpty(t *testing.T, p storage.Provider) {
	if got := listKeys(t, p, ""); len(got) != 0 {
		t.Errorf("List(\"\") on empty provider = %q, want empty", got)
	}
	put(t, p, "clips/a/1.json", []byte("x"))
	if got := listKeys(t, p, "clips/missing"); len(got) != 0 {
		t.Errorf("List(clips/missing) = %q, want empty", got)
	}
	if got := listKeys(t, p, "nothing/here/at/all"); len(got) != 0 {
		t.Errorf("List(nothing/here/at/all) = %q, want empty", got)
	}
}

func testListExactKey(t *testing.T, p storage.Provider) {
	put(t, p, "status/relay.json", []byte("x"))
	put(t, p, "status/relay.json.old", []byte("x"))
	got := listKeys(t, p, "status/relay.json")
	if want := []string{"status/relay.json"}; !slices.Equal(got, want) {
		t.Errorf("List(status/relay.json) = %q, want %q", got, want)
	}
}

func testListOmitsDotNames(t *testing.T, p storage.Provider) {
	for _, k := range []string{
		"clips/a/1.json",
		"clips/a/.1.json.part",
		"clips/a/.hidden",
		".top",
	} {
		put(t, p, k, []byte("x"))
	}
	got := listKeys(t, p, "")
	if want := []string{"clips/a/1.json"}; !slices.Equal(got, want) {
		t.Errorf("List(\"\") = %q, want %q", got, want)
	}
	got = listKeys(t, p, "clips/a")
	if want := []string{"clips/a/1.json"}; !slices.Equal(got, want) {
		t.Errorf("List(clips/a) = %q, want %q", got, want)
	}
	if got := listKeys(t, p, "clips/a/.hidden"); len(got) != 0 {
		t.Errorf("List(clips/a/.hidden) = %q, want empty", got)
	}
}

func testListEarlyBreakSafe(t *testing.T, p storage.Provider) {
	for i := range 5 {
		put(t, p, "clips/a/"+string(rune('a'+i))+".json", []byte("x"))
	}
	n := 0
	for _, err := range p.List(t.Context(), "clips") {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("iterated %d entries before break, want 2", n)
	}
	// The provider is still usable afterwards.
	if got := listKeys(t, p, "clips"); len(got) != 5 {
		t.Errorf("List after early break = %d entries, want 5", len(got))
	}
}

func testListInvalidPrefix(t *testing.T, p storage.Provider) {
	for _, prefix := range []string{"../x", "/abs", "a//b", "a\\b"} {
		var errs []error
		n := 0
		for _, err := range p.List(t.Context(), prefix) {
			n++
			if err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) != 1 || n != 1 || !errors.Is(errs[0], storage.ErrInvalidKey) {
			t.Errorf("List(%q) yielded %d entries, errors %v; want exactly one ErrInvalidKey", prefix, n, errs)
		}
	}
}

func testDotNamesExplicitAccess(t *testing.T, p storage.Provider) {
	const key = "clips/a/.145959123_01JC0000000000000000000000.json.part"
	data := []byte(`{"clip_id":"01JC0000000000000000000000"}`)
	put(t, p, key, data)

	info, err := p.Stat(t.Context(), key)
	if err != nil {
		t.Fatalf("Stat(dot-name): %v", err)
	}
	if info.Key != key || info.Size != int64(len(data)) {
		t.Errorf("Stat(dot-name) = {%q %d}, want {%q %d}", info.Key, info.Size, key, len(data))
	}
	if got := readAll(t, p, key); !bytes.Equal(got, data) {
		t.Errorf("OpenRange(dot-name) = %q, want %q", got, data)
	}
	if err := p.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete(dot-name): %v", err)
	}
	if _, err := p.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after Delete error = %v, want ErrNotFound", err)
	}
}

func testDelete(t *testing.T, p storage.Provider) {
	const key = "clips/a/1.json"
	put(t, p, key, []byte("x"))
	put(t, p, "clips/a/2.json", []byte("y"))
	if err := p.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after Delete error = %v, want ErrNotFound", err)
	}
	if got := listKeys(t, p, ""); !slices.Equal(got, []string{"clips/a/2.json"}) {
		t.Errorf("List after Delete = %q, want [clips/a/2.json]", got)
	}
	// Deleting a missing object is not an error (S3 DeleteObject semantics).
	if err := p.Delete(t.Context(), key); err != nil {
		t.Errorf("Delete(missing) error = %v, want nil", err)
	}
}

func testPutSmallOverwriteChangesETag(t *testing.T, p storage.Provider) {
	const key = "status/relay.json"
	first := []byte(`{"state":"aaaa"}`)
	second := []byte(`{"state":"bbbb"}`) // same size, different content
	put(t, p, key, first)
	a, err := p.Stat(t.Context(), key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	put(t, p, key, second)
	b, err := p.Stat(t.Context(), key)
	if err != nil {
		t.Fatalf("Stat after overwrite: %v", err)
	}
	if a.ETag == b.ETag {
		t.Errorf("ETag unchanged after overwrite with new content: %q", a.ETag)
	}
	if got := readAll(t, p, key); !bytes.Equal(got, second) {
		t.Errorf("content after overwrite = %q, want %q", got, second)
	}
	if got := listKeys(t, p, ""); !slices.Equal(got, []string{key}) {
		t.Errorf("List after overwrite = %q, want [%s]", got, key)
	}
}

func testPutSmallLimit(t *testing.T, p storage.Provider) {
	const limit = 1 << 20
	ok := pattern(limit, 2)
	put(t, p, "status/max.json", ok)
	if got := readAll(t, p, "status/max.json"); !bytes.Equal(got, ok) {
		t.Errorf("1 MiB PutSmall read back %d bytes, want %d matching", len(got), len(ok))
	}
	if err := p.PutSmall(t.Context(), "status/big.json", pattern(limit+1, 3), "application/json"); err == nil {
		t.Error("PutSmall(1 MiB + 1) error = nil, want error")
	}
	if _, err := p.Stat(t.Context(), "status/big.json"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after oversized PutSmall error = %v, want ErrNotFound", err)
	}
}

func testCreateVisibleOnlyAfterCommit(t *testing.T, p storage.Provider) {
	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000000.zrec"
	ctx := t.Context()
	data := pattern(300_000, 4)
	w, err := p.Create(ctx, key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeChunks(t, w, data, 64<<10, 0)

	if _, err := p.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat before Commit error = %v, want ErrNotFound", err)
	}
	if got := listKeys(t, p, ""); len(got) != 0 {
		t.Errorf("List before Commit = %q, want empty", got)
	}

	if err := w.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	info, err := p.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat after Commit: %v", err)
	}
	if info.Size != int64(len(data)) {
		t.Errorf("committed Size = %d, want %d", info.Size, len(data))
	}
	if got := readAll(t, p, key); !bytes.Equal(got, data) {
		t.Errorf("committed content differs (%d bytes read, want %d)", len(got), len(data))
	}
	if got := listKeys(t, p, ""); !slices.Equal(got, []string{key}) {
		t.Errorf("List after Commit = %q, want [%s]", got, key)
	}
	if _, err := w.Write([]byte("late")); err == nil {
		t.Error("Write after Commit error = nil, want error")
	}
}

func testAbortLeavesNothingVisible(t *testing.T, p storage.Provider) {
	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000001.zrec"
	ctx := t.Context()
	w, err := p.Create(ctx, key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeChunks(t, w, pattern(100_000, 5), 32<<10, 0)
	if err := w.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := p.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after Abort error = %v, want ErrNotFound", err)
	}
	if got := listKeys(t, p, ""); len(got) != 0 {
		t.Errorf("List after Abort = %q, want empty", got)
	}
	if err := w.Commit(ctx); err == nil {
		t.Error("Commit after Abort error = nil, want error")
	}
	if _, err := p.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after Commit-after-Abort error = %v, want ErrNotFound", err)
	}
}

func testResumeContinuesExactBytes(t *testing.T, p storage.Provider) {
	const key = "clips/cam/2026/11/06/145959123_01JC0000000000000000000002.zrec"
	ctx := context.Background()
	// Large enough to span several multipart parts on drivers that use them.
	data := pattern(11<<20+7, 6)
	w, err := p.Create(ctx, key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	written := writeChunks(t, w, data[:9<<20+3], 1<<20, 0)

	for range 2 { // Resume is safe to call repeatedly.
		d := w.DurableOffset()
		off, err := w.Resume(ctx)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if off < d {
			t.Fatalf("Resume = %d, below last DurableOffset %d", off, d)
		}
		if off > written {
			t.Fatalf("Resume = %d, beyond bytes written %d", off, written)
		}
		if got := w.DurableOffset(); got < d {
			t.Fatalf("DurableOffset after Resume = %d, below %d", got, d)
		}
		written = off
	}

	writeChunks(t, w, data[written:], 1<<20, written)
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got := readAll(t, p, key)
	if !bytes.Equal(got, data) {
		t.Fatalf("committed object after Resume differs: %d bytes, want %d", len(got), len(data))
	}
}
