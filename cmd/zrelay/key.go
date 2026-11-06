package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/mdp/qrterminal/v3"

	"github.com/apikdech/zrelay-v2/pkg/secretref"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const keyUsage = `usage: zrelay key <command>

commands:
  key generate [--qr]               print a new master key, its fingerprint and an optional QR code
  key fingerprint --key <ref>       print key_id
  key api-token-hash --key <ref>    print hex(SHA-256(K_api)) for zview-server
`

// randReader is the CSPRNG "key generate" reads from; tests replace it.
var randReader io.Reader = rand.Reader

// keyCmd dispatches "zrelay key ..." (relay-spec §6).
func keyCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, keyUsage)
		return exitInvalid
	}
	switch args[0] {
	case "generate":
		return keyGenerate(args[1:], stdout, stderr)
	case "fingerprint":
		return keyFromRef("fingerprint", args[1:], stdout, stderr, func(m zrec.MasterKey) string {
			return zrec.FormatKeyID(zrec.Derive(m).ID)
		})
	case "api-token-hash":
		return keyFromRef("api-token-hash", args[1:], stdout, stderr, func(m zrec.MasterKey) string {
			// The preimage is the raw 32 bytes of K_api (Contract §3.2).
			api := zrec.Derive(m).API
			sum := sha256.Sum256(api[:])
			return hex.EncodeToString(sum[:])
		})
	default:
		fmt.Fprint(stderr, keyUsage)
		return exitInvalid
	}
}

// keyGenerate implements "zrelay key generate [--qr]": it prints a new
// master key and its fingerprint, and with --qr the uppercase form as a
// terminal QR code (Contract §3.1).
func keyGenerate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("zrelay key generate", stderr)
	qr := fs.Bool("qr", false, "also print the key as a terminal QR code")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: zrelay key generate [--qr]")
		return exitInvalid
	}
	m, err := zrec.GenerateMasterKey(randReader)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: key generate: %v\n", err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "key:         %s\n", m)
	fmt.Fprintf(stdout, "fingerprint: %s\n", zrec.FormatKeyID(zrec.Derive(m).ID))
	if *qr {
		// Uppercase so QR alphanumeric mode applies and the code stays small.
		qrterminal.GenerateWithConfig(m.Upper(), qrterminal.Config{
			Level:          qrterminal.M,
			Writer:         stdout,
			HalfBlocks:     true,
			QuietZone:      1,
			BlackChar:      qrterminal.BLACK_BLACK,
			WhiteChar:      qrterminal.WHITE_WHITE,
			BlackWhiteChar: qrterminal.BLACK_WHITE,
			WhiteBlackChar: qrterminal.WHITE_BLACK,
		})
	}
	return exitOK
}

// keyFromRef implements the "--key <ref>" subcommands: resolve the ref
// (never a literal), parse the master key and print what derive returns.
func keyFromRef(name string, args []string, stdout, stderr io.Writer, derive func(zrec.MasterKey) string) int {
	fs := newFlagSet("zrelay key "+name, stderr)
	ref := fs.String("key", "", "master key `ref` (ENV:NAME or FILE:/path), never a literal")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *ref == "" || fs.NArg() != 0 {
		fmt.Fprintf(stderr, "usage: zrelay key %s --key <ref>\n", name)
		return exitInvalid
	}
	if !secretref.IsRef(*ref) {
		// Never echo the value: it may be the key itself.
		fmt.Fprintf(stderr, "zrelay: key %s: --key takes a reference (ENV:NAME or FILE:/path), never a literal\n", name)
		return exitInvalid
	}
	text, err := secretref.Resolve(*ref, false)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: key %s: %v\n", name, err)
		return exitInvalid
	}
	m, err := zrec.ParseMasterKey(text)
	if err != nil {
		fmt.Fprintf(stderr, "zrelay: key %s: %v\n", name, err)
		return exitInvalid
	}
	fmt.Fprintln(stdout, derive(m))
	return exitOK
}
