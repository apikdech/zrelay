package storagetest

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// recoverWait bounds how long the suite polls for a crashed writer to age past
// a tiny threshold. Drivers whose timestamps come from a remote server (S3
// Initiated) may need a moment of clock skew to pass.
const recoverWait = 10 * time.Second

// RunRecoverer executes the Recoverer conformance suite (Contract §7.4).
// newProvider returns a fresh, empty provider together with its Recoverer.
// crash abandons a writer without Commit or Abort, simulating kill -9: it must
// release the writer's resources while leaving its staged data in place.
func RunRecoverer(
	t *testing.T,
	newProvider func(t *testing.T) (storage.Provider, storage.Recoverer),
	crash func(t *testing.T, w storage.ObjectWriter),
) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, p storage.Provider, r storage.Recoverer, crash func(*testing.T, storage.ObjectWriter))
	}{
		{"YoungerNotListed", testYoungerNotListed},
		{"OlderListedAndRecovered", testOlderListedAndRecovered},
		{"PrefixByWholeElement", testIncompletePrefixByWholeElement},
		{"NothingToRecover", testNothingToRecover},
		{"StagingSidecarNotListed", testStagingSidecarNotListed},
		{"ListStagingReturnsDotNamedSidecars", testListStaging},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, r := newProvider(t)
			t.Cleanup(func() { _ = p.Close() })
			c.fn(t, p, r, crash)
		})
	}
}

const recoverKey = "clips/cam/2026/11/06/120000000_01JC0000000000000000000009.zrec"

// crashedWrite creates key, writes data and crashes the writer. It returns the
// last DurableOffset observed before the crash.
func crashedWrite(t *testing.T, p storage.Provider, key string, data []byte, crash func(*testing.T, storage.ObjectWriter)) int64 {
	t.Helper()
	w, err := p.Create(t.Context(), key)
	if err != nil {
		t.Fatalf("Create(%q): %v", key, err)
	}
	writeChunks(t, w, data, 1<<20, 0)
	d := w.DurableOffset()
	crash(t, w)
	return d
}

func findIncomplete(t *testing.T, r storage.Recoverer, prefix string, olderThan time.Duration, key string) (storage.Incomplete, bool) {
	t.Helper()
	incs, err := r.ListIncomplete(t.Context(), prefix, olderThan)
	if err != nil {
		t.Fatalf("ListIncomplete(%q, %v): %v", prefix, olderThan, err)
	}
	for _, inc := range incs {
		if inc.Key == key {
			return inc, true
		}
	}
	return storage.Incomplete{}, false
}

// waitIncomplete polls until key is listed as incomplete with a tiny age
// threshold, failing the test after recoverWait.
func waitIncomplete(t *testing.T, r storage.Recoverer, prefix, key string) storage.Incomplete {
	t.Helper()
	deadline := time.Now().Add(recoverWait)
	for {
		if inc, ok := findIncomplete(t, r, prefix, time.Millisecond, key); ok {
			return inc
		}
		if time.Now().After(deadline) {
			t.Fatalf("ListIncomplete(%q) never listed %q", prefix, key)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func testYoungerNotListed(t *testing.T, p storage.Provider, r storage.Recoverer, crash func(*testing.T, storage.ObjectWriter)) {
	crashedWrite(t, p, recoverKey, pattern(100_000, 7), crash)
	if inc, ok := findIncomplete(t, r, "clips/cam", time.Hour, recoverKey); ok {
		t.Errorf("ListIncomplete(olderThan 1h) listed a fresh crashed writer: %+v", inc)
	}
	if _, err := p.Stat(t.Context(), recoverKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(crashed key) error = %v, want ErrNotFound", err)
	}
}

func testOlderListedAndRecovered(t *testing.T, p storage.Provider, r storage.Recoverer, crash func(*testing.T, storage.ObjectWriter)) {
	// Large enough that part-based drivers have at least one completed part.
	data := pattern(11<<20+123, 8)
	d := crashedWrite(t, p, recoverKey, data, crash)

	inc := waitIncomplete(t, r, "clips/cam", recoverKey)
	if inc.Size < 0 || inc.Size > int64(len(data)) {
		t.Errorf("Incomplete.Size = %d, want within [0, %d]", inc.Size, len(data))
	}
	if inc.ModTime.IsZero() || inc.ModTime.Location() != time.UTC {
		t.Errorf("Incomplete.ModTime = %v, want non-zero UTC", inc.ModTime)
	}
	if _, err := p.Stat(t.Context(), recoverKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat before RecoverCommit error = %v, want ErrNotFound", err)
	}

	// 11 MiB+123 completes at least one part on part-based drivers, so a
	// driver that reports nothing durable here is broken, not exempt.
	if d <= 0 {
		t.Fatalf("DurableOffset before the crash = %d after writing %d bytes, want > 0", d, len(data))
	}

	info, err := r.RecoverCommit(t.Context(), inc)
	if err != nil {
		t.Fatalf("RecoverCommit: %v", err)
	}
	if info.Key != recoverKey {
		t.Errorf("RecoverCommit Key = %q, want %q", info.Key, recoverKey)
	}
	if info.Size < d || info.Size > int64(len(data)) {
		t.Fatalf("recovered Size = %d, want within [DurableOffset %d, %d]", info.Size, d, len(data))
	}
	st, err := p.Stat(t.Context(), recoverKey)
	if err != nil {
		t.Fatalf("Stat after RecoverCommit: %v", err)
	}
	if st.Size != info.Size {
		t.Errorf("Stat Size = %d, RecoverCommit Size = %d", st.Size, info.Size)
	}
	if got := readAll(t, p, recoverKey); !bytes.Equal(got, data[:info.Size]) {
		t.Errorf("recovered content is not the durable prefix (%d bytes)", len(got))
	}
	if got := listKeys(t, p, "clips/cam"); !slices.Equal(got, []string{recoverKey}) {
		t.Errorf("List after RecoverCommit = %q, want [%s]", got, recoverKey)
	}
	if _, ok := findIncomplete(t, r, "clips/cam", 0, recoverKey); ok {
		t.Error("ListIncomplete still lists the key after RecoverCommit")
	}
}

func testIncompletePrefixByWholeElement(t *testing.T, p storage.Provider, r storage.Recoverer, crash func(*testing.T, storage.ObjectWriter)) {
	const other = "clips/front-door/2026/11/06/120000000_01JC000000000000000000000A.zrec"
	const mine = "clips/front/2026/11/06/120000000_01JC000000000000000000000B.zrec"
	crashedWrite(t, p, other, pattern(1000, 9), crash)
	crashedWrite(t, p, mine, pattern(1000, 10), crash)

	waitIncomplete(t, r, "clips/front", mine)
	if _, ok := findIncomplete(t, r, "clips/front", time.Millisecond, other); ok {
		t.Errorf("ListIncomplete(clips/front) listed %q", other)
	}
	waitIncomplete(t, r, "", other)
	if _, ok := findIncomplete(t, r, "clips/missing", 0, mine); ok {
		t.Errorf("ListIncomplete(clips/missing) listed %q", mine)
	}
	if _, err := r.ListIncomplete(t.Context(), "../x", 0); !errors.Is(err, storage.ErrInvalidKey) {
		t.Errorf("ListIncomplete(../x) error = %v, want ErrInvalidKey", err)
	}
}

func testNothingToRecover(t *testing.T, p storage.Provider, r storage.Recoverer, crash func(*testing.T, storage.ObjectWriter)) {
	crashedWrite(t, p, recoverKey, nil, crash)
	inc := waitIncomplete(t, r, "clips/cam", recoverKey)
	if _, err := r.RecoverCommit(t.Context(), inc); !errors.Is(err, storage.ErrNothingToRecover) {
		t.Fatalf("RecoverCommit(empty) error = %v, want ErrNothingToRecover", err)
	}
	if _, err := p.Stat(t.Context(), recoverKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat after empty RecoverCommit error = %v, want ErrNotFound", err)
	}
	if _, ok := findIncomplete(t, r, "clips/cam", 0, recoverKey); ok {
		t.Error("ListIncomplete still lists an empty upload after RecoverCommit")
	}
}

// testStagingSidecarNotListed pins the cross-driver rule for staging sidecars
// (Contract §7.3 step 0): ".<name>.json.part" is an ordinary object written by
// PutSmall, not a crashed stream. s3 never lists it (it is not a multipart
// upload), so no driver may: recovery promotes staging sidecars by its own
// rules (Contract §7.4), never through RecoverCommit.
func testStagingSidecarNotListed(t *testing.T, p storage.Provider, r storage.Recoverer, crash func(*testing.T, storage.ObjectWriter)) {
	const dir = "clips/cam/2026/11/06/"
	const sidecar = dir + "120000000_01JC0000000000000000000009.json"
	const staging = dir + ".120000000_01JC0000000000000000000009.json.part"
	put(t, p, staging, []byte(`{"clip_id":"01JC0000000000000000000009"}`))
	put(t, p, "status/.relay.json.part", []byte(`{}`))
	crashedWrite(t, p, recoverKey, pattern(1000, 11), crash)

	// The crashed stream proves the listing has aged past the threshold.
	waitIncomplete(t, r, "", recoverKey)
	incs, err := r.ListIncomplete(t.Context(), "", time.Millisecond)
	if err != nil {
		t.Fatalf("ListIncomplete: %v", err)
	}
	for _, inc := range incs {
		if inc.Key != recoverKey {
			t.Errorf("ListIncomplete listed %+v, want only the crashed stream %q", inc, recoverKey)
		}
	}
	if _, ok := findIncomplete(t, r, sidecar, 0, sidecar); ok {
		t.Errorf("ListIncomplete(%q) listed the staging sidecar", sidecar)
	}
	if got := readAll(t, p, staging); string(got) != `{"clip_id":"01JC0000000000000000000009"}` {
		t.Errorf("staging sidecar changed: %q", got)
	}
}

// testListStaging pins ListStaging (Contract §7.3 step 0): it returns exactly
// the dot-named staging sidecars ".<name>.json.part" under a prefix, by whole
// path element, which List hides. Final objects, other dot-names and crashed
// video streams are not staging sidecars.
func testListStaging(t *testing.T, p storage.Provider, r storage.Recoverer, crash func(*testing.T, storage.ObjectWriter)) {
	const (
		mine      = "clips/front/2026/11/06/.120000000_01JC000000000000000000000B.json.part"
		mineOther = "clips/front/2026/11/05/.130000000_01JC000000000000000000000C.json.part"
		other     = "clips/front-door/2026/11/06/.120000000_01JC000000000000000000000A.json.part"
	)
	body := []byte(`{"clip_id":"x"}`)
	put(t, p, mine, body)
	put(t, p, mineOther, body)
	put(t, p, other, body)
	put(t, p, "clips/front/2026/11/06/120000000_01JC000000000000000000000B.json", []byte(`{}`))
	put(t, p, "clips/front/2026/11/06/.stray.tmp", []byte(`{}`))
	crashedWrite(t, p, "clips/front/2026/11/06/121212000_01JC000000000000000000000D.zrec", pattern(1000, 12), crash)

	keys := func(prefix string) []string {
		t.Helper()
		infos, err := r.ListStaging(t.Context(), prefix)
		if err != nil {
			t.Fatalf("ListStaging(%q): %v", prefix, err)
		}
		var ks []string
		for _, in := range infos {
			if in.Size != int64(len(body)) {
				t.Errorf("ListStaging(%q) %q Size = %d, want %d", prefix, in.Key, in.Size, len(body))
			}
			if in.ModTime.IsZero() || in.ModTime.Location() != time.UTC {
				t.Errorf("ListStaging(%q) %q ModTime = %v, want non-zero UTC", prefix, in.Key, in.ModTime)
			}
			ks = append(ks, in.Key)
		}
		slices.Sort(ks)
		return ks
	}
	if got, want := keys("clips/front"), []string{mineOther, mine}; !slices.Equal(got, want) {
		t.Errorf("ListStaging(clips/front) = %q, want %q", got, want)
	}
	if got, want := keys("clips/front/2026/11/06"), []string{mine}; !slices.Equal(got, want) {
		t.Errorf("ListStaging(day) = %q, want %q", got, want)
	}
	if got, want := keys(""), []string{other, mineOther, mine}; !slices.Equal(got, want) {
		t.Errorf("ListStaging(\"\") = %q, want %q", got, want)
	}
	if got := keys("clips/missing"); len(got) != 0 {
		t.Errorf("ListStaging(clips/missing) = %q, want none", got)
	}
	if _, err := r.ListStaging(t.Context(), "../x"); !errors.Is(err, storage.ErrInvalidKey) {
		t.Errorf("ListStaging(../x) error = %v, want ErrInvalidKey", err)
	}
	// List still hides them (Contract §7.2).
	for _, k := range listKeys(t, p, "clips/front") {
		if k == mine || k == mineOther {
			t.Errorf("List returned staging sidecar %q", k)
		}
	}
}
