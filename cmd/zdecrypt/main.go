// Command zdecrypt decrypts, verifies and rewraps zrec clips (relay-spec §6).
//
//	zdecrypt --key <ref> --in <file.zrec> [--out <file.mp4 | ->] [--sidecar <file.json>] [--allow-incomplete]
//	zdecrypt rewrap --from <ref> --to <ref> --in <file.zrec> --out <file.zrec>
//
// Every key flag takes an ENV: or FILE: reference, never a literal, and --key
// may be repeated to build a keyring. Plaintext is released one chunk at a
// time, only after that chunk's tag verifies.
//
// Exit codes: 0 OK, 1 other error, 2 authentication failure, 3 truncated,
// 4 trailing data, 5 unknown key, 6 sidecar/header mismatch.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/apikdech/zrelay-v2/pkg/secretref"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// Exit codes (relay-spec §6).
const (
	exitOK       = 0
	exitOther    = 1
	exitAuth     = 2
	exitTrunc    = 3
	exitTrailing = 4
	exitUnknown  = 5
	exitMismatch = 6
)

const usage = `usage:
  zdecrypt [--env-file <path>] --key <ref> --in <file.zrec> [--out <file.mp4 | ->] [--sidecar <file.json>] [--allow-incomplete]
  zdecrypt rewrap [--env-file <path>] --from <ref> --to <ref> --in <file.zrec> --out <file.zrec>

<ref> is ENV:NAME or FILE:/path, never the key itself. --key may be repeated.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to decrypt or rewrap and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "rewrap" {
		return rewrap(args[1:], stderr)
	}
	return decrypt(args, stdout, stderr)
}

// refList is a repeatable flag holding key references.
type refList []string

func (l *refList) String() string     { return "" }
func (l *refList) Set(v string) error { *l = append(*l, v); return nil }

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

// parse parses args into fs; ok is false when the caller must exit with code.
func parse(fs *flag.FlagSet, args []string) (code int, ok bool) {
	switch err := fs.Parse(args); {
	case err == nil:
		return exitOK, true
	case errors.Is(err, flag.ErrHelp):
		return exitOK, false
	default:
		return exitOther, false
	}
}

func fail(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "zdecrypt: "+format+"\n", args...)
	return exitOther
}

// loadKeys resolves refs to a keyring. Errors name references and schemes,
// never key material.
func loadKeys(flagName string, refs []string) (zrec.Keyring, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("--%s is required", flagName)
	}
	ring := make(zrec.Keyring, len(refs))
	for _, ref := range refs {
		s, err := secretref.Resolve(ref, false)
		if errors.Is(err, secretref.ErrLiteral) {
			return nil, fmt.Errorf("--%s must be an ENV: or FILE: reference, not the key itself", flagName)
		}
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", flagName, err)
		}
		m, err := zrec.ParseMasterKey(s)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", flagName, err)
		}
		k := zrec.Derive(m)
		ring[k.ID] = k
	}
	return ring, nil
}

// exitCode maps a zrec error to its exit code.
func exitCode(err error) int {
	switch {
	case errors.Is(err, zrec.ErrAuth), errors.Is(err, zrec.ErrAuthHeader):
		return exitAuth
	case errors.Is(err, zrec.ErrTruncated):
		return exitTrunc
	case errors.Is(err, zrec.ErrTrailingData):
		return exitTrailing
	case errors.Is(err, zrec.ErrUnknownKey):
		return exitUnknown
	default:
		return exitOther
	}
}

func decrypt(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("zdecrypt", stderr)
	var keys refList
	fs.Var(&keys, "key", "key reference (ENV:NAME or FILE:/path); repeatable")
	in := fs.String("in", "", "encrypted clip `file.zrec`")
	out := fs.String("out", "", "plaintext output `file`, or - for stdout (default: --in with .zrec replaced by .mp4)")
	sidecarPath := fs.String("sidecar", "", "sidecar `file.json` to cross-check against the header")
	allowIncomplete := fs.Bool("allow-incomplete", false, "accept a clip that ends mid-chunk or lacks its final chunk")
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 || *in == "" {
		fmt.Fprint(stderr, usage)
		return exitOther
	}
	if *envFile != "" {
		if err := secretref.LoadEnvFile(*envFile); err != nil {
			return fail(stderr, "%v", err)
		}
	}
	ring, err := loadKeys("key", keys)
	if err != nil {
		return fail(stderr, "%v", err)
	}

	f, err := os.Open(*in)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fail(stderr, "%v", err)
	}

	var sc *sidecar.Sidecar
	if *sidecarPath != "" {
		if sc, err = readSidecar(*sidecarPath); err != nil {
			return fail(stderr, "sidecar: %v", err)
		}
		if sc.Status == "incomplete" {
			*allowIncomplete = true
		}
	}

	r, err := zrec.OpenReader(f, fi.Size(), ring, zrec.ReaderOptions{AllowIncomplete: *allowIncomplete})
	if err != nil {
		fmt.Fprintf(stderr, "zdecrypt: %v\n", err)
		return exitCode(err)
	}
	if sc != nil {
		if err := checkSidecar(sc, r.Header()); err != nil {
			fmt.Fprintf(stderr, "zdecrypt: sidecar does not match the clip header: %v\n", err)
			return exitMismatch
		}
	}

	dst, removeOnFail, code := openOutput(*in, *out, stdout, stderr)
	if dst == nil {
		return code
	}
	n, err := r.WriteTo(dst)
	closeErr := closeOutput(dst, *out)
	if errors.Is(err, zrec.ErrTruncated) && *allowIncomplete && n > 0 {
		fmt.Fprintf(stderr, "zdecrypt: warning: clip is incomplete (%v); wrote %d verified bytes\n", err, n)
		err = nil
	}
	if err == nil && closeErr != nil {
		return failOutput(stderr, removeOnFail, closeErr, exitOther)
	}
	if err != nil {
		return failOutput(stderr, removeOnFail, err, exitCode(err))
	}
	return exitOK
}

// failOutput reports err, deletes the partial output file (if any) and
// returns code.
func failOutput(stderr io.Writer, removeOnFail string, err error, code int) int {
	if removeOnFail != "" {
		os.Remove(removeOnFail)
	}
	fmt.Fprintf(stderr, "zdecrypt: %v\n", err)
	return code
}

// openOutput opens the plaintext destination. removeOnFail is the path to
// delete if decryption fails, empty for stdout. A nil writer means failure,
// with code the exit code.
func openOutput(in, out string, stdout, stderr io.Writer) (w io.Writer, removeOnFail string, code int) {
	if out == "-" {
		return stdout, "", exitOK
	}
	if out == "" {
		out = strings.TrimSuffix(in, ".zrec") + ".mp4"
	}
	if sameFile(in, out) {
		return nil, "", fail(stderr, "--out must not be the input file")
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, "", fail(stderr, "%v", err)
	}
	return f, out, exitOK
}

// closeOutput closes dst if it is a file we opened.
func closeOutput(dst io.Writer, out string) error {
	if f, ok := dst.(*os.File); ok && out != "-" {
		return f.Close()
	}
	return nil
}

// sameFile reports whether a and b name the same existing file.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}

func readSidecar(path string) (*sidecar.Sidecar, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sc, err := sidecar.Decode(b)
	if err != nil {
		return nil, err
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	return sc, nil
}

// checkSidecar compares the fields the sidecar copies from the header. The
// header is authoritative (Contract §4.4); any difference is a mismatch.
func checkSidecar(sc *sidecar.Sidecar, h zrec.Header) error {
	var bad []string
	if sc.Camera.ID != h.CameraID {
		bad = append(bad, "camera.id")
	}
	if sc.ClipID != h.ClipID {
		bad = append(bad, "clip_id")
	}
	if sc.StartTime.UnixMilli() != h.StartUnixMs {
		bad = append(bad, "start_time")
	}
	if !strings.EqualFold(sc.Video.KeyID, fmt.Sprintf("%x", h.KeyID)) {
		bad = append(bad, "video.key_id")
	}
	if sc.Video.ChunkSize != int64(h.ChunkSize) {
		bad = append(bad, "video.chunk_size")
	}
	if len(bad) > 0 {
		return fmt.Errorf("differs in %s", strings.Join(bad, ", "))
	}
	return nil
}

func rewrap(args []string, stderr io.Writer) int {
	fs := newFlagSet("zdecrypt rewrap", stderr)
	var from, to refList
	fs.Var(&from, "from", "old key reference (ENV:NAME or FILE:/path); repeatable")
	fs.Var(&to, "to", "new key reference (ENV:NAME or FILE:/path)")
	in := fs.String("in", "", "encrypted clip `file.zrec`")
	out := fs.String("out", "", "rewrapped clip `file.zrec`")
	envFile := fs.String("env-file", "", "load KEY=VALUE lines into the environment before resolving ENV: references")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 || *in == "" || *out == "" {
		fmt.Fprint(stderr, usage)
		return exitOther
	}
	if *envFile != "" {
		if err := secretref.LoadEnvFile(*envFile); err != nil {
			return fail(stderr, "%v", err)
		}
	}
	fromRing, err := loadKeys("from", from)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if len(to) != 1 {
		return fail(stderr, "--to takes exactly one key reference")
	}
	toRing, err := loadKeys("to", to)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	var toKeys zrec.Keys
	for _, k := range toRing {
		toKeys = k
	}

	f, err := os.Open(*in)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if sameFile(*in, *out) {
		return fail(stderr, "--out must not be the input file")
	}
	dst, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	err = zrec.Rewrap(f, fi.Size(), dst, fromRing, toKeys, nil)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(*out)
		fmt.Fprintf(stderr, "zdecrypt: %v\n", err)
		return exitCode(err)
	}
	return exitOK
}
