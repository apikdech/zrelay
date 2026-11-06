package storage

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CleanKey validates an object key and returns it unchanged. Every driver
// passes every key through it (relay-spec §4.9). A key is a non-empty
// sequence of "/"-separated elements; it is rejected (ErrInvalidKey) when it
// is absolute, contains an empty, "." or ".." element, a backslash, a control
// character or invalid UTF-8. A leading "." in an element is permitted,
// because crash recovery addresses staging objects by explicit key (Contract
// §7.2).
func CleanKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	if !utf8.ValidString(key) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrInvalidKey)
	}
	for _, r := range key {
		if r == '\\' {
			return "", fmt.Errorf("%w: %q contains a backslash", ErrInvalidKey, key)
		}
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %q contains a control character", ErrInvalidKey, key)
		}
	}
	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("%w: %q is absolute", ErrInvalidKey, key)
	}
	for elem := range strings.SplitSeq(key, "/") {
		switch elem {
		case "":
			return "", fmt.Errorf("%w: %q has an empty path element", ErrInvalidKey, key)
		case ".", "..":
			return "", fmt.Errorf("%w: %q has a %q path element", ErrInvalidKey, key, elem)
		}
	}
	return key, nil
}

// JoinRoot prefixes key with the storage root (Contract §7.1). Both are
// expected to be clean; an empty root leaves key unchanged and an empty key
// yields the root itself.
func JoinRoot(root, key string) string {
	switch {
	case root == "":
		return key
	case key == "":
		return root
	default:
		return root + "/" + key
	}
}
