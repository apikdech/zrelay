package zrec

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func fixedMaster() MasterKey {
	var m MasterKey
	for i := range m {
		m[i] = byte(i)
	}
	return m
}

func TestKeyTextRoundTrip(t *testing.T) {
	m, err := GenerateMasterKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	s := m.String()
	if !strings.HasPrefix(s, "zk1-") || len(s) != len("zk1-")+64+1+4 {
		t.Fatalf("String() = %q, want zk1-<64hex>-<4hex>", s)
	}
	if s != strings.ToLower(s) {
		t.Fatalf("String() not lowercase: %q", s)
	}
	if m.Upper() != strings.ToUpper(s) {
		t.Fatalf("Upper() = %q, want %q", m.Upper(), strings.ToUpper(s))
	}
	got, err := ParseMasterKey(s)
	if err != nil {
		t.Fatalf("ParseMasterKey(String()): %v", err)
	}
	if got != m {
		t.Fatal("round trip mismatch")
	}

	// Checksum is SHA-256("zk1" || key)[0:2].
	f := fixedMaster()
	sum := sha256.Sum256(append([]byte("zk1"), f[:]...))
	want := "zk1-" + hex.EncodeToString(f[:]) + "-" + hex.EncodeToString(sum[:2])
	if f.String() != want {
		t.Fatalf("String() = %q, want %q", f.String(), want)
	}
}

func TestGenerateMasterKeyShortReader(t *testing.T) {
	if _, err := GenerateMasterKey(bytes.NewReader(make([]byte, 31))); err == nil {
		t.Fatal("GenerateMasterKey with 31-byte reader: want error")
	}
}

func TestParseKeyCaseAndWhitespaceInsensitive(t *testing.T) {
	m := fixedMaster()
	s := m.String()
	variants := []string{
		m.Upper(),
		"  " + s + "\n",
		"\t" + s[:20] + " \n " + s[20:40] + "\r\n" + s[40:] + " ",
		strings.ToUpper(s[:30]) + s[30:],
	}
	for _, v := range variants {
		got, err := ParseMasterKey(v)
		if err != nil {
			t.Fatalf("ParseMasterKey(%q): %v", v, err)
		}
		if got != m {
			t.Fatalf("ParseMasterKey(%q) returned wrong key", v)
		}
	}
}

func TestParseKeyBadChecksumIsTypo(t *testing.T) {
	s := fixedMaster().String()
	last := s[len(s)-1]
	flip := byte('0')
	if last == '0' {
		flip = '1'
	}
	bad := s[:len(s)-1] + string(flip)
	_, err := ParseMasterKey(bad)
	if !errors.Is(err, ErrKeyChecksum) {
		t.Fatalf("ParseMasterKey(bad checksum) err = %v, want ErrKeyChecksum", err)
	}
	if errors.Is(err, ErrKeyFormat) {
		t.Fatal("bad checksum must not be reported as ErrKeyFormat")
	}

	// Flipping a key hex digit also yields a checksum mismatch, not a format error.
	kflip := byte('f')
	if s[10] == 'f' {
		kflip = 'e'
	}
	badKey := s[:10] + string(kflip) + s[11:]
	if _, err := ParseMasterKey(badKey); !errors.Is(err, ErrKeyChecksum) {
		t.Fatalf("ParseMasterKey(flipped key digit) err = %v, want ErrKeyChecksum", err)
	}
}

func TestParseKeyBadFormat(t *testing.T) {
	s := fixedMaster().String()
	cases := []string{
		"",
		"zk2" + s[3:],
		s[:len(s)-1],
		s + "0",
		strings.Replace(s, "-", "_", 1),
		s[:4] + "g" + s[5:],
		s[:len(s)-1] + "z",
		"zk1-" + strings.Repeat("0", 64),
	}
	for _, c := range cases {
		_, err := ParseMasterKey(c)
		if !errors.Is(err, ErrKeyFormat) {
			t.Errorf("ParseMasterKey(%q) err = %v, want ErrKeyFormat", c, err)
		}
	}
}

func TestDeriveFixedMaster(t *testing.T) {
	m := fixedMaster()
	salt := []byte("zrec/v1")
	ref := func(info string, n int) []byte {
		b, err := hkdf.Key(sha256.New, m[:], salt, info, n)
		if err != nil {
			t.Fatalf("hkdf.Key(%s): %v", info, err)
		}
		return b
	}
	wantID := ref("key-id", 8)
	wantWrap := ref("wrap", 32)
	wantAPI := ref("api-token", 32)
	wantHash := sha256.Sum256(wantAPI)

	k := Derive(m)
	if !bytes.Equal(k.ID[:], wantID) {
		t.Errorf("ID = %x, want %x", k.ID, wantID)
	}
	if !bytes.Equal(k.Wrap[:], wantWrap) {
		t.Errorf("Wrap = %x, want %x", k.Wrap, wantWrap)
	}
	if !bytes.Equal(k.API[:], wantAPI) {
		t.Errorf("API = %x, want %x", k.API, wantAPI)
	}
	if h := APITokenHash(k); h != wantHash {
		t.Errorf("APITokenHash = %x, want %x", h, wantHash)
	}

	tok := BearerToken(k)
	if strings.Contains(tok, "=") {
		t.Errorf("BearerToken padded: %q", tok)
	}
	if tok != base64.RawURLEncoding.EncodeToString(wantAPI) {
		t.Errorf("BearerToken = %q, want unpadded base64url of K_api", tok)
	}
}

func TestFormatKeyID(t *testing.T) {
	id := [8]byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07, 0x18}
	if got := FormatKeyID(id); got != "a1b2-c3d4-e5f6-0718" {
		t.Fatalf("FormatKeyID = %q, want a1b2-c3d4-e5f6-0718", got)
	}
	if got := FormatKeyID([8]byte{}); got != "0000-0000-0000-0000" {
		t.Fatalf("FormatKeyID(zero) = %q", got)
	}
}
