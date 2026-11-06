package bearer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func token(b byte, n int) []byte {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = b + byte(i)
	}
	return raw
}

func hashHex(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func TestBearerAcceptsValidRejectsPaddedAndShort(t *testing.T) {
	cur := token(1, 32)
	next := token(100, 32)
	v, err := New([]string{hashHex(cur), strings.ToUpper(hashHex(next))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString

	if !v.Check("Bearer " + enc(cur)) {
		t.Error("valid token rejected")
	}
	if !v.Check("Bearer " + enc(next)) {
		t.Error("second rotation hash rejected")
	}
	if v.Check("Bearer " + base64.URLEncoding.EncodeToString(cur)) {
		t.Error("padded token accepted")
	}
	short := token(1, 31)
	if v.Check("Bearer " + enc(short)) {
		t.Error("31-byte token accepted")
	}
	// A 31-byte token whose hash is stored must still be rejected on length.
	vs, err := New([]string{hashHex(short)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if vs.Check("Bearer " + enc(short)) {
		t.Error("31-byte token accepted even though its hash is stored")
	}
	if v.Check("Bearer " + enc(token(7, 32))) {
		t.Error("wrong key accepted")
	}
	if v.Check("Bearer " + enc(token(1, 33))) {
		t.Error("33-byte token accepted")
	}
	for _, h := range []string{"", "Bearer", "Bearer ", enc(cur), "Basic " + enc(cur), "Bearer  " + enc(cur), "Bearer " + enc(cur) + " "} {
		if v.Check(h) {
			t.Errorf("Check(%q) = true", h)
		}
	}
}

func TestBearerRejectsStdAlphabet(t *testing.T) {
	// 0xfb 0xff encodes to "-_" in base64url and "+/" in std base64.
	raw := append([]byte{0xfb, 0xff, 0xff}, token(9, 29)...)
	v, err := New([]string{hashHex(raw)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !v.Check("Bearer " + base64.RawURLEncoding.EncodeToString(raw)) {
		t.Fatal("base64url token rejected")
	}
	if v.Check("Bearer " + base64.RawStdEncoding.EncodeToString(raw)) {
		t.Fatal("standard-alphabet token accepted")
	}
}

func TestNewValidatesHashes(t *testing.T) {
	good := hashHex(token(1, 32))
	bad := [][]string{
		nil,
		{},
		{good, good, good},
		{good[:63]},
		{good + "0"},
		{"zz" + good[2:]},
	}
	for _, hs := range bad {
		if _, err := New(hs); err == nil {
			t.Errorf("New(%q): want error", hs)
		}
	}
}
