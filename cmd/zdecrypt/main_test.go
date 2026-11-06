package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const vectorDir = "../../testdata/zrec/v1"

// vectorMaster is the Contract §4.5 master key: bytes 0x00..0x1f.
func vectorMaster() zrec.MasterKey {
	var m zrec.MasterKey
	for i := range m {
		m[i] = byte(i)
	}
	return m
}

// keyRef writes m to a 0600 file under dir and returns its FILE: reference.
func keyRef(t *testing.T, m zrec.MasterKey) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(m.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return "FILE:" + p
}

func vectorPath(name string) string { return filepath.Join(vectorDir, name) }

// plaintextSHA returns the manifest's plaintext_sha256_hex for a positive vector.
func plaintextSHA(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(vectorPath("vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Positive []struct {
			Name   string `json:"name"`
			SHA256 string `json:"plaintext_sha256_hex"`
		} `json:"positive"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Positive {
		if p.Name == name {
			return p.SHA256
		}
	}
	t.Fatalf("no positive vector %q", name)
	return ""
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// sidecarFor writes a valid complete sidecar matching the vector clip
// (camera, clip_id, key_id, chunk_size), after applying edit.
func sidecarFor(t *testing.T, status string, edit func(*sidecar.Sidecar)) string {
	t.Helper()
	fixture := "valid_complete.json"
	if status == "incomplete" {
		fixture = "valid_incomplete.json"
	}
	b, err := os.ReadFile(filepath.Join("../../testdata/sidecars", fixture))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sidecar.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	k := zrec.Derive(vectorMaster())
	sc.Video.KeyID = hex.EncodeToString(k.ID[:])
	sc.Video.ChunkSize = 65536
	if edit != nil {
		edit(sc)
	}
	out, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "clip.json")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestZdecryptExitCodes(t *testing.T) {
	key := keyRef(t, vectorMaster())
	wrongKey := keyRef(t, zrec.MasterKey{1})
	mismatch := sidecarFor(t, "complete", func(s *sidecar.Sidecar) { s.Camera.ID = "garage" })
	matching := sidecarFor(t, "complete", nil)

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"positive", []string{"--key", key, "--in", vectorPath("pos-two-chunks.zrec")}, 0},
		{"positive with matching sidecar", []string{"--key", key, "--in", vectorPath("pos-two-chunks.zrec"), "--sidecar", matching}, 0},
		{"bitflip chunk", []string{"--key", key, "--in", vectorPath("neg-bitflip-chunk.zrec")}, 2},
		{"header authentication failure", []string{"--key", key, "--in", vectorPath("neg-wrapped-dek-bitflip.zrec")}, 2},
		{"dropped final chunk", []string{"--key", key, "--in", vectorPath("neg-dropped-final-chunk.zrec")}, 3},
		{"extra chunk", []string{"--key", key, "--in", vectorPath("neg-extra-chunk.zrec")}, 4},
		{"wrong key", []string{"--key", wrongKey, "--in", vectorPath("pos-two-chunks.zrec")}, 5},
		{"sidecar camera mismatch", []string{"--key", key, "--in", vectorPath("pos-two-chunks.zrec"), "--sidecar", mismatch}, 6},
		{"missing input", []string{"--key", key, "--in", vectorPath("nope.zrec")}, 1},
		{"missing --in", []string{"--key", key}, 1},
		{"missing --key", []string{"--in", vectorPath("pos-two-chunks.zrec")}, 1},
		{"unsupported key scheme", []string{"--key", "KEYCHAIN:zrelay", "--in", vectorPath("pos-two-chunks.zrec")}, 1},
		{"unset ENV key", []string{"--key", "ENV:ZDECRYPT_TEST_UNSET", "--in", vectorPath("pos-two-chunks.zrec")}, 1},
		{"not a zrec file", []string{"--key", key, "--in", "../../go.mod"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := c.args
			if !contains(args, "--out") {
				args = append(args[:len(args):len(args)], "--out", filepath.Join(t.TempDir(), "out.mp4"))
			}
			code, _, stderr := runCLI(args...)
			if code != c.want {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, c.want, stderr)
			}
			if c.name == "positive" {
				if got, want := fileSHA(t, args[len(args)-1]), plaintextSHA(t, "two-chunks"); got != want {
					t.Fatalf("output sha256 = %s, want %s", got, want)
				}
			}
		})
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestZdecryptWritesVerifiedPlaintext(t *testing.T) {
	key := keyRef(t, vectorMaster())
	out := filepath.Join(t.TempDir(), "out.mp4")
	code, _, stderr := runCLI("--key", key, "--in", vectorPath("pos-two-and-half-chunks.zrec"), "--out", out)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if got, want := fileSHA(t, out), plaintextSHA(t, "two-and-half-chunks"); got != want {
		t.Fatalf("output sha256 = %s, want %s", got, want)
	}
}

func TestZdecryptRemovesOutputOnFailure(t *testing.T) {
	key := keyRef(t, vectorMaster())
	out := filepath.Join(t.TempDir(), "out.mp4")
	code, _, _ := runCLI("--key", key, "--in", vectorPath("neg-bitflip-chunk.zrec"), "--out", out)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("partial output left behind (stat err: %v)", err)
	}
}

func TestZdecryptStdout(t *testing.T) {
	key := keyRef(t, vectorMaster())
	code, stdout, stderr := runCLI("--key", key, "--in", vectorPath("pos-two-chunks.zrec"), "--out", "-")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	sum := sha256.Sum256([]byte(stdout))
	if got, want := hex.EncodeToString(sum[:]), plaintextSHA(t, "two-chunks"); got != want {
		t.Fatalf("stdout sha256 = %s, want %s", got, want)
	}
	if stderr != "" {
		t.Fatalf("stderr not empty on success: %q", stderr)
	}
}

func TestZdecryptStdoutFailureReleasesOnlyVerifiedChunks(t *testing.T) {
	key := keyRef(t, vectorMaster())
	// neg-bitflip-chunk has chunk 1 damaged: chunk 0 (64 KiB) may be released,
	// nothing after it.
	code, stdout, _ := runCLI("--key", key, "--in", vectorPath("neg-bitflip-chunk.zrec"), "--out", "-")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if len(stdout) > 65536 {
		t.Fatalf("released %d plaintext bytes, more than the verified first chunk", len(stdout))
	}
}

func TestZdecryptAllowIncomplete(t *testing.T) {
	key := keyRef(t, vectorMaster())
	in := vectorPath("neg-dropped-final-chunk.zrec")

	out := filepath.Join(t.TempDir(), "partial.mp4")
	code, _, stderr := runCLI("--key", key, "--in", in, "--out", out, "--allow-incomplete")
	if code != 0 {
		t.Fatalf("--allow-incomplete exit = %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "incomplete") {
		t.Fatalf("expected a warning about the incomplete clip, got %q", stderr)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("verified plaintext was not kept: %v", err)
	}

	// An incomplete sidecar implies --allow-incomplete.
	inc := sidecarFor(t, "incomplete", nil)
	out2 := filepath.Join(t.TempDir(), "partial2.mp4")
	if code, _, stderr := runCLI("--key", key, "--in", in, "--out", out2, "--sidecar", inc); code != 0 {
		t.Fatalf("incomplete sidecar exit = %d: %s", code, stderr)
	}

	// A complete sidecar does not.
	comp := sidecarFor(t, "complete", nil)
	if code, _, _ := runCLI("--key", key, "--in", in, "--out", filepath.Join(t.TempDir(), "x"), "--sidecar", comp); code != 3 {
		t.Fatalf("complete sidecar exit = %d, want 3", code)
	}

	// Allow-incomplete does not excuse a header-only clip: nothing to recover.
	if code, _, _ := runCLI("--key", key, "--in", vectorPath("neg-header-only.zrec"), "--out", filepath.Join(t.TempDir(), "y"), "--allow-incomplete"); code != 3 {
		t.Fatalf("header-only exit = %d, want 3", code)
	}

	// It does not excuse corruption in an earlier chunk.
	if code, _, _ := runCLI("--key", key, "--in", vectorPath("neg-bitflip-chunk.zrec"), "--out", filepath.Join(t.TempDir(), "z"), "--allow-incomplete"); code != 2 {
		t.Fatalf("bitflip exit = %d, want 2", code)
	}
}

func TestZdecryptSidecarMismatches(t *testing.T) {
	key := keyRef(t, vectorMaster())
	for name, edit := range map[string]func(*sidecar.Sidecar){
		"clip_id":    func(s *sidecar.Sidecar) { s.ClipID[15] ^= 1 },
		"start_time": func(s *sidecar.Sidecar) { s.StartTime = sidecar.Time{Time: s.StartTime.Add(time.Millisecond)} },
	} {
		t.Run(name, func(t *testing.T) {
			sc := sidecarFor(t, "complete", edit)
			code, _, stderr := runCLI("--key", key, "--in", vectorPath("pos-two-chunks.zrec"),
				"--out", filepath.Join(t.TempDir(), "o"), "--sidecar", sc)
			if code != 6 {
				t.Fatalf("exit = %d, want 6; stderr: %s", code, stderr)
			}
		})
	}
	t.Run("unreadable sidecar", func(t *testing.T) {
		code, _, _ := runCLI("--key", key, "--in", vectorPath("pos-two-chunks.zrec"),
			"--out", filepath.Join(t.TempDir(), "o"), "--sidecar", filepath.Join(t.TempDir(), "none.json"))
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
	})
}

// Contract §6.1: video.key_id and video.chunk_size are hints only; the
// descriptor is authoritative. A stale hint warns but does not fail (G3 limits
// exit 6 to camera_id, clip_id and start_unix_ms).
func TestZdecryptStaleSidecarHintsWarnButDecrypt(t *testing.T) {
	key := keyRef(t, vectorMaster())
	for name, edit := range map[string]func(*sidecar.Sidecar){
		"key_id":     func(s *sidecar.Sidecar) { s.Video.KeyID = "0000000000000000" },
		"chunk_size": func(s *sidecar.Sidecar) { s.Video.ChunkSize = 131072 },
	} {
		t.Run(name, func(t *testing.T) {
			sc := sidecarFor(t, "complete", edit)
			out := filepath.Join(t.TempDir(), "o")
			code, _, stderr := runCLI("--key", key, "--in", vectorPath("pos-two-chunks.zrec"), "--out", out, "--sidecar", sc)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "video."+name) {
				t.Fatalf("stderr lacks a warning naming video.%s: %q", name, stderr)
			}
			if got, want := fileSHA(t, out), plaintextSHA(t, "two-chunks"); got != want {
				t.Fatalf("output sha256 = %s, want %s", got, want)
			}
		})
	}
}

func TestZdecryptRejectsLiteralKey(t *testing.T) {
	m := vectorMaster()
	code, stdout, stderr := runCLI("--key", m.String(), "--in", vectorPath("pos-two-chunks.zrec"), "--out", "-")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatal("plaintext released despite a literal key")
	}
	if strings.Contains(stderr, m.String()) || strings.Contains(stderr, hex.EncodeToString(m[:])) {
		t.Fatalf("stderr leaks the key: %q", stderr)
	}
	if !strings.Contains(stderr, "ENV:") || !strings.Contains(stderr, "FILE:") {
		t.Fatalf("stderr should point at ENV:/FILE: references, got %q", stderr)
	}
}

func TestZdecryptKeyringAndEnvFile(t *testing.T) {
	key := keyRef(t, vectorMaster())
	other := keyRef(t, zrec.MasterKey{9})

	// Repeated --key builds a keyring; the matching key is selected by key_id.
	if code, _, stderr := runCLI("--key", other, "--key", key, "--in", vectorPath("pos-one-byte.zrec"), "--out", "-"); code != 0 {
		t.Fatalf("keyring exit = %d: %s", code, stderr)
	}

	// --env-file feeds an ENV: reference.
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("ZDECRYPT_TEST_KEY="+vectorMaster().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("ZDECRYPT_TEST_KEY") })
	if code, _, stderr := runCLI("--env-file", envFile, "--key", "ENV:ZDECRYPT_TEST_KEY", "--in", vectorPath("pos-one-byte.zrec"), "--out", "-"); code != 0 {
		t.Fatalf("env-file exit = %d: %s", code, stderr)
	}
}

func TestZdecryptDefaultOutputName(t *testing.T) {
	key := keyRef(t, vectorMaster())
	dir := t.TempDir()
	src, err := os.ReadFile(vectorPath("pos-one-byte.zrec"))
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "clip.zrec")
	if err := os.WriteFile(in, src, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI("--key", key, "--in", in); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if got, want := fileSHA(t, filepath.Join(dir, "clip.mp4")), plaintextSHA(t, "one-byte"); got != want {
		t.Fatalf("clip.mp4 sha256 = %s, want %s", got, want)
	}
}

func TestZdecryptRefusesToOverwriteInput(t *testing.T) {
	key := keyRef(t, vectorMaster())
	dir := t.TempDir()
	src, _ := os.ReadFile(vectorPath("pos-one-byte.zrec"))
	in := filepath.Join(dir, "clip.zrec")
	if err := os.WriteFile(in, src, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI("--key", key, "--in", in, "--out", in); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got, _ := os.ReadFile(in); !bytes.Equal(got, src) {
		t.Fatal("input was modified")
	}
}

func TestZdecryptRewrap(t *testing.T) {
	oldKey := keyRef(t, vectorMaster())
	newMaster := zrec.MasterKey{42}
	newKey := keyRef(t, newMaster)
	dir := t.TempDir()
	rewrapped := filepath.Join(dir, "new.zrec")

	code, _, stderr := runCLI("rewrap", "--from", oldKey, "--to", newKey,
		"--in", vectorPath("pos-two-and-half-chunks.zrec"), "--out", rewrapped)
	if code != 0 {
		t.Fatalf("rewrap exit = %d: %s", code, stderr)
	}

	// Decrypts with the new key to the same plaintext, and not with the old.
	out := filepath.Join(dir, "out.mp4")
	if code, _, stderr := runCLI("--key", newKey, "--in", rewrapped, "--out", out); code != 0 {
		t.Fatalf("decrypt rewrapped exit = %d: %s", code, stderr)
	}
	if got, want := fileSHA(t, out), plaintextSHA(t, "two-and-half-chunks"); got != want {
		t.Fatalf("plaintext sha256 = %s, want %s", got, want)
	}
	if code, _, _ := runCLI("--key", oldKey, "--in", rewrapped, "--out", "-"); code != 5 {
		t.Fatalf("old key on rewrapped clip: exit = %d, want 5", code)
	}

	// Chunk bytes are identical to the original.
	orig, _ := os.ReadFile(vectorPath("pos-two-and-half-chunks.zrec"))
	got, _ := os.ReadFile(rewrapped)
	hdr, err := zrec.ParseHeader(orig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(orig[hdr.HeaderLen:], got[hdr.HeaderLen:]) {
		t.Fatal("rewrap changed the chunks")
	}
}

// Rewrap changes the header key_id but nothing rewrites the sidecar, so a
// rewrapped clip must still decrypt with its original sidecar (Contract §3.4, §6.1).
func TestZdecryptRewrappedClipWithOriginalSidecar(t *testing.T) {
	oldKey := keyRef(t, vectorMaster())
	newKey := keyRef(t, zrec.MasterKey{42})
	dir := t.TempDir()
	rewrapped := filepath.Join(dir, "new.zrec")
	sc := sidecarFor(t, "complete", nil) // key_id hint names the OLD key
	if code, _, stderr := runCLI("rewrap", "--from", oldKey, "--to", newKey,
		"--in", vectorPath("pos-two-chunks.zrec"), "--out", rewrapped); code != 0 {
		t.Fatalf("rewrap exit = %d: %s", code, stderr)
	}
	out := filepath.Join(dir, "out.mp4")
	code, _, stderr := runCLI("--key", newKey, "--in", rewrapped, "--sidecar", sc, "--out", out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got, want := fileSHA(t, out), plaintextSHA(t, "two-chunks"); got != want {
		t.Fatalf("output sha256 = %s, want %s", got, want)
	}
}

func TestZdecryptRewrapFailures(t *testing.T) {
	oldKey := keyRef(t, vectorMaster())
	newKey := keyRef(t, zrec.MasterKey{42})
	wrong := keyRef(t, zrec.MasterKey{7})
	in := vectorPath("pos-two-chunks.zrec")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"wrong from key", []string{"rewrap", "--from", wrong, "--to", newKey, "--in", in}, 5},
		{"damaged key block", []string{"rewrap", "--from", oldKey, "--to", newKey, "--in", vectorPath("neg-wrapped-dek-bitflip.zrec")}, 2},
		{"literal to key", []string{"rewrap", "--from", oldKey, "--to", zrec.MasterKey{42}.String(), "--in", in}, 1},
		{"missing --to", []string{"rewrap", "--from", oldKey, "--in", in}, 1},
		{"missing input", []string{"rewrap", "--from", oldKey, "--to", newKey, "--in", vectorPath("nope.zrec")}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.zrec")
			code, _, stderr := runCLI(append(c.args, "--out", out)...)
			if code != c.want {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, c.want, stderr)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("output left behind after failure (stat err: %v)", err)
			}
		})
	}

	t.Run("out equals in", func(t *testing.T) {
		if code, _, _ := runCLI("rewrap", "--from", oldKey, "--to", newKey, "--in", in, "--out", in); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
	})
}
