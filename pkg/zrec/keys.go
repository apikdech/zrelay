// Package zrec implements the zrec key material and container format
// (Contract §3, §4).
package zrec

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// MasterKey is the 32-byte household master key (Contract §3.1).
type MasterKey [32]byte

var (
	// ErrKeyFormat reports text that is not of the form zk1-<64hex>-<4hex>.
	ErrKeyFormat = errors.New("zrec: malformed master key")
	// ErrKeyChecksum reports a well-formed key whose checksum does not
	// match: almost certainly a typo, not a different key.
	ErrKeyChecksum = errors.New("zrec: master key checksum mismatch (typo?)")
)

const (
	keyPrefix  = "zk1"
	keyTextLen = len(keyPrefix) + 1 + 64 + 1 + 4

	hkdfSalt     = "zrec/v1"
	infoKeyID    = "key-id"
	infoWrap     = "wrap"
	infoAPIToken = "api-token"
)

func keyChecksum(m MasterKey) [2]byte {
	h := sha256.New()
	h.Write([]byte(keyPrefix))
	h.Write(m[:])
	var sum [2]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// String returns the canonical lowercase text form zk1-<64hex>-<4hex>.
func (m MasterKey) String() string {
	sum := keyChecksum(m)
	return keyPrefix + "-" + hex.EncodeToString(m[:]) + "-" + hex.EncodeToString(sum[:])
}

// Upper returns the uppercase text form, used for QR codes so that QR
// alphanumeric mode applies.
func (m MasterKey) Upper() string {
	return strings.ToUpper(m.String())
}

// ParseMasterKey parses the text form. It is case-insensitive and ignores
// all whitespace. A bad checksum yields ErrKeyChecksum; anything else that
// is malformed yields ErrKeyFormat. Errors never include key material.
func ParseMasterKey(s string) (MasterKey, error) {
	var m MasterKey
	s = strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s))
	if len(s) != keyTextLen {
		return m, fmt.Errorf("%w: want %d characters, got %d", ErrKeyFormat, keyTextLen, len(s))
	}
	if !strings.HasPrefix(s, keyPrefix+"-") || s[len(keyPrefix)+1+64] != '-' {
		return m, fmt.Errorf("%w: want zk1-<64 hex>-<4 hex>", ErrKeyFormat)
	}
	body := s[len(keyPrefix)+1 : len(keyPrefix)+1+64]
	if _, err := hex.Decode(m[:], []byte(body)); err != nil {
		return MasterKey{}, fmt.Errorf("%w: key is not hex", ErrKeyFormat)
	}
	var got [2]byte
	if _, err := hex.Decode(got[:], []byte(s[len(s)-4:])); err != nil {
		return MasterKey{}, fmt.Errorf("%w: checksum is not hex", ErrKeyFormat)
	}
	if got != keyChecksum(m) {
		return MasterKey{}, ErrKeyChecksum
	}
	return m, nil
}

// GenerateMasterKey reads 32 bytes from r (a CSPRNG such as crypto/rand.Reader).
func GenerateMasterKey(r io.Reader) (MasterKey, error) {
	var m MasterKey
	if _, err := io.ReadFull(r, m[:]); err != nil {
		return MasterKey{}, fmt.Errorf("zrec: generate master key: %w", err)
	}
	return m, nil
}

// Keys holds the values derived from a master key (Contract §3.2).
type Keys struct {
	ID   [8]byte
	Wrap [32]byte
	API  [32]byte
}

func derive(m MasterKey, info string, out []byte) {
	b, err := hkdf.Key(sha256.New, m[:], []byte(hkdfSalt), info, len(out))
	if err != nil {
		// Only possible for lengths beyond 255*32 bytes.
		panic("zrec: hkdf: " + err.Error())
	}
	copy(out, b)
	clear(b)
}

// Derive computes key_id, K_wrap and K_api with HKDF-SHA256, salt "zrec/v1".
func Derive(m MasterKey) Keys {
	var k Keys
	derive(m, infoKeyID, k.ID[:])
	derive(m, infoWrap, k.Wrap[:])
	derive(m, infoAPIToken, k.API[:])
	return k
}

// FormatKeyID renders a key_id as a fingerprint, e.g. a1b2-c3d4-e5f6-0718.
func FormatKeyID(id [8]byte) string {
	h := hex.EncodeToString(id[:])
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16]
}

// APITokenHash returns SHA-256 over the raw 32 bytes of K_api, the value
// zview-server stores as its verifier.
func APITokenHash(k Keys) [32]byte {
	return sha256.Sum256(k.API[:])
}

// BearerToken returns the bearer credential: unpadded base64url of K_api.
func BearerToken(k Keys) string {
	return base64.RawURLEncoding.EncodeToString(k.API[:])
}
