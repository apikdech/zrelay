// Package bearer verifies K_api bearer credentials (Contract §3.2).
package bearer

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	maxHashes = 2
	tokenLen  = 32
	// Unpadded base64url length of a 32-byte token.
	encodedLen = (tokenLen*8 + 5) / 6
)

// Verifier checks bearer credentials against up to two stored
// hex(SHA-256(K_api)) values (two allow key rotation).
type Verifier struct {
	hashes [][sha256.Size]byte
}

// New builds a Verifier from 1–2 hex-encoded SHA-256 hashes.
func New(hexHashes []string) (*Verifier, error) {
	if len(hexHashes) < 1 || len(hexHashes) > maxHashes {
		return nil, fmt.Errorf("bearer: want 1 to %d token hashes, got %d", maxHashes, len(hexHashes))
	}
	v := &Verifier{hashes: make([][sha256.Size]byte, len(hexHashes))}
	for i, h := range hexHashes {
		if len(h) != 2*sha256.Size {
			return nil, fmt.Errorf("bearer: token hash %d: want %d hex characters, got %d", i, 2*sha256.Size, len(h))
		}
		if _, err := hex.Decode(v.hashes[i][:], []byte(h)); err != nil {
			return nil, fmt.Errorf("bearer: token hash %d is not hex", i)
		}
	}
	return v, nil
}

// Check reports whether authorizationHeader is "Bearer <token>" where
// <token> is unpadded base64url of exactly 32 bytes whose SHA-256 matches a
// stored hash. Every stored hash is compared in constant time.
func (v *Verifier) Check(authorizationHeader string) bool {
	const scheme = "Bearer "
	if len(authorizationHeader) < len(scheme) || !strings.EqualFold(authorizationHeader[:len(scheme)], scheme) {
		return false
	}
	tok := authorizationHeader[len(scheme):]
	// The length check also rejects padding and stray characters, which the
	// decoder would otherwise skip (it ignores \r and \n).
	if len(tok) != encodedLen {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(tok)
	if err != nil || len(raw) != tokenLen {
		return false
	}
	sum := sha256.Sum256(raw)
	ok := 0
	for i := range v.hashes {
		ok |= subtle.ConstantTimeCompare(sum[:], v.hashes[i][:])
	}
	return ok == 1
}
