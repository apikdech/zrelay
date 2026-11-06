package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/mdp/qrterminal/v3"

	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// seqMaster is the master key 00 01 02 … 1f.
func seqMaster() zrec.MasterKey {
	var m zrec.MasterKey
	for i := range m {
		m[i] = byte(i)
	}
	return m
}

// withSeqRand makes "key generate" read seqMaster's bytes.
func withSeqRand(t *testing.T) {
	t.Helper()
	old := randReader
	m := seqMaster()
	randReader = bytes.NewReader(m[:])
	t.Cleanup(func() { randReader = old })
}

func TestKeyGenerate(t *testing.T) {
	withSeqRand(t)
	code, out, errOut := runCLI("key", "generate")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	m := seqMaster()
	for _, want := range []string{m.String(), zrec.FormatKeyID(zrec.Derive(m).ID)} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\u2588") || strings.Contains(out, "\u2580") {
		t.Errorf("stdout has a QR code without --qr:\n%s", out)
	}
}

func TestKeyGenerateQRUsesUppercaseForm(t *testing.T) {
	withSeqRand(t)
	code, out, errOut := runCLI("key", "generate", "--qr")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	m := seqMaster()
	var want bytes.Buffer
	qrterminal.GenerateWithConfig(m.Upper(), qrterminal.Config{
		Level: qrterminal.M, Writer: &want, HalfBlocks: true, QuietZone: 1,
		BlackChar: qrterminal.BLACK_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE, WhiteBlackChar: qrterminal.WHITE_BLACK,
	})
	if want.Len() == 0 || !strings.Contains(out, want.String()) {
		t.Errorf("stdout lacks the QR code of the uppercase key:\n%s", out)
	}
	if !strings.Contains(out, m.String()) {
		t.Errorf("stdout lacks the text key:\n%s", out)
	}
}

func TestKeyGenerateRejectsExtraArgs(t *testing.T) {
	if code, _, _ := runCLI("key", "generate", "extra"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestKeyFingerprint(t *testing.T) {
	m := seqMaster()
	t.Setenv("ZR_TEST_KEY", m.String())
	code, out, errOut := runCLI("key", "fingerprint", "--key", "ENV:ZR_TEST_KEY")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	want := zrec.FormatKeyID(zrec.Derive(m).ID) + "\n"
	if out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
}

func TestKeyFingerprintFileRef(t *testing.T) {
	m := seqMaster()
	p := writeFile(t, t.TempDir(), "master", m.Upper()+"\n")
	code, out, errOut := runCLI("key", "fingerprint", "--key", "FILE:"+p)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	if want := zrec.FormatKeyID(zrec.Derive(m).ID) + "\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
}

func TestKeyAPITokenHash(t *testing.T) {
	m := seqMaster()
	t.Setenv("ZR_TEST_KEY", m.String())
	code, out, errOut := runCLI("key", "api-token-hash", "--key", "ENV:ZR_TEST_KEY")
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	api := zrec.Derive(m).API
	sum := sha256.Sum256(api[:])
	if want := hex.EncodeToString(sum[:]) + "\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
}

func TestKeyCLIRejectsLiteral(t *testing.T) {
	lit := seqMaster().String()
	for _, sub := range []string{"fingerprint", "api-token-hash"} {
		t.Run(sub, func(t *testing.T) {
			code, out, errOut := runCLI("key", sub, "--key", lit)
			if code == 0 {
				t.Fatalf("exit 0, want non-zero")
			}
			if !strings.Contains(errOut, "reference") || !strings.Contains(errOut, "never a literal") {
				t.Errorf("stderr does not say refs only:\n%s", errOut)
			}
			if strings.Contains(errOut+out, lit) {
				t.Errorf("the literal key leaked into the output")
			}
		})
	}
}

func TestKeyCLIUnsupportedSchemeIsNotCalledLiteral(t *testing.T) {
	for _, sub := range []string{"fingerprint", "api-token-hash"} {
		t.Run(sub, func(t *testing.T) {
			code, _, errOut := runCLI("key", sub, "--key", "KEYCHAIN:zrelay")
			if code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if strings.Contains(errOut, "never a literal") {
				t.Errorf("a KEYCHAIN: reference was called a literal:\n%s", errOut)
			}
			if !strings.Contains(errOut, "unsupported") {
				t.Errorf("stderr does not say the scheme is unsupported:\n%s", errOut)
			}
		})
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("entropy source broke") }

func TestKeyGenerateRandFailureExitsOne(t *testing.T) {
	old := randReader
	randReader = failReader{}
	t.Cleanup(func() { randReader = old })
	code, out, errOut := runCLI("key", "generate")
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr:\n%s", code, errOut)
	}
	if out != "" {
		t.Errorf("stdout not empty on failure:\n%s", out)
	}
	if !strings.Contains(errOut, "entropy source broke") {
		t.Errorf("stderr lacks the cause:\n%s", errOut)
	}
}

func TestKeyCLIBadKeys(t *testing.T) {
	good := seqMaster().String()
	badSum := good[:len(good)-1] + map[bool]string{true: "1", false: "0"}[good[len(good)-1] == '0']
	for name, tc := range map[string]struct{ val, want string }{
		"checksum typo": {badSum, "typo"},
		"bad format":    {"not-a-key", "malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ZR_TEST_KEY", tc.val)
			code, _, errOut := runCLI("key", "fingerprint", "--key", "ENV:ZR_TEST_KEY")
			if code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, errOut)
			}
		})
	}
}

func TestKeyCLIUsage(t *testing.T) {
	for _, args := range [][]string{
		{"key"},
		{"key", "bogus"},
		{"key", "fingerprint"},
		{"key", "api-token-hash"},
		{"key", "fingerprint", "--key", "ENV:X", "extra"},
	} {
		if code, _, _ := runCLI(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	t.Setenv("ZR_UNSET_KEY_VAR", "")
	if code, _, _ := runCLI("key", "fingerprint", "--key", "ENV:ZR_UNSET_KEY_VAR"); code != 2 {
		t.Errorf("empty ENV ref: exit %d, want 2", code)
	}
}

func TestConfigCheckPrintsKeyFingerprint(t *testing.T) {
	setExampleEnv(t)
	code, out, errOut := runCLI("config", "check", "--config", exampleConfigFile(t, nil))
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr:\n%s", code, errOut)
	}
	want := "key fingerprint: " + zrec.FormatKeyID(zrec.Derive(seqMaster()).ID) + "\n"
	if !strings.Contains(out, want) {
		t.Errorf("stdout lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, seqMaster().String()) {
		t.Errorf("stdout contains the master key")
	}
}

func TestConfigCheckRejectsBadMasterKey(t *testing.T) {
	setExampleEnv(t)
	cfg := exampleConfigFile(t, nil)
	writeFile(t, cfg[:strings.LastIndex(cfg, "/")], "zrelay_master", "zk1-example-master-key\n")
	code, out, errOut := runCLI("config", "check", "--config", cfg)
	if code != 2 {
		t.Fatalf("exit %d, want 2; stdout:\n%s", code, out)
	}
	if !strings.Contains(errOut, "master key") {
		t.Errorf("stderr does not name the master key:\n%s", errOut)
	}
	if strings.Contains(errOut, "zk1-example-master-key") {
		t.Errorf("stderr leaks the key text:\n%s", errOut)
	}
}
