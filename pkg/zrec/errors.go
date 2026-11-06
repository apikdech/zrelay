package zrec

import (
	"errors"
	"strconv"
)

// Errors reported by the zrec writer and reader (Contract §4.3–4.4).
var (
	// ErrFormat reports an unsupported or malformed header.
	ErrFormat = errors.New("zrec: bad format")
	// ErrUnknownKey reports a key_id that is not in the keyring.
	ErrUnknownKey = errors.New("zrec: unknown key_id")
	// ErrAuthHeader reports a DEK that fails to unwrap.
	ErrAuthHeader = errors.New("zrec: header authentication failed")
	// ErrTruncated reports a clip that ends early.
	ErrTruncated = errors.New("zrec: truncated")
	// ErrTrailingData reports chunks after the final chunk.
	ErrTrailingData = errors.New("zrec: trailing data after final chunk")
	// ErrEmpty reports a clip with no plaintext.
	ErrEmpty = errors.New("zrec: empty plaintext")
	// ErrAuth reports a chunk that fails authentication; see AuthError.
	ErrAuth = errors.New("zrec: chunk authentication failed")
)

// AuthError reports which chunk failed authentication. It matches ErrAuth
// under errors.Is.
type AuthError struct{ Chunk int }

func (e *AuthError) Error() string {
	return ErrAuth.Error() + ": chunk " + strconv.Itoa(e.Chunk)
}

// Is reports whether target is ErrAuth.
func (e *AuthError) Is(target error) bool { return target == ErrAuth }
