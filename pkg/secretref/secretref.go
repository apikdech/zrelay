// Package secretref resolves secret references (relay-spec §5.1 as amended
// by design §11): "ENV:NAME" reads an environment variable and "FILE:/path"
// reads a file with one trailing newline trimmed. Any other scheme, including
// "KEYCHAIN:", is unsupported. Literal values are rejected unless the caller
// allows them.
//
// Errors and log records name variables and paths, never secret values.
package secretref

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

const (
	envPrefix  = "ENV:"
	filePrefix = "FILE:"
)

var (
	// ErrLiteral is returned when a literal value is given and literals are
	// not allowed.
	ErrLiteral = errors.New("secretref: literal secrets are not allowed; use an ENV: or FILE: reference")
	// ErrUnsupported is returned for any reference scheme other than ENV:
	// and FILE:, including KEYCHAIN:.
	ErrUnsupported = errors.New("secretref: unsupported reference scheme")
)

// IsRef reports whether s is a supported reference, that is, whether it
// starts with "ENV:" or "FILE:".
func IsRef(s string) bool {
	return strings.HasPrefix(s, envPrefix) || strings.HasPrefix(s, filePrefix)
}

// Resolve returns the value that ref refers to. A string that is neither a
// supported reference nor shaped like one (an upper-case scheme followed by
// a colon) is a literal: it is returned as is when allowLiteral is true and
// rejected with ErrLiteral otherwise.
func Resolve(ref string, allowLiteral bool) (string, error) {
	switch {
	case strings.HasPrefix(ref, envPrefix):
		return resolveEnv(strings.TrimPrefix(ref, envPrefix))
	case strings.HasPrefix(ref, filePrefix):
		return resolveFile(strings.TrimPrefix(ref, filePrefix))
	}
	if scheme, ok := schemeOf(ref); ok {
		return "", fmt.Errorf("%w %q", ErrUnsupported, scheme+":")
	}
	if !allowLiteral {
		return "", ErrLiteral
	}
	return ref, nil
}

func resolveEnv(name string) (string, error) {
	if name == "" {
		return "", errors.New("secretref: ENV: reference has an empty variable name")
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("secretref: environment variable %q is not set", name)
	}
	return v, nil
}

func resolveFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("secretref: FILE: reference has an empty path")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("secretref: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("secretref: %w", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		slog.Warn("secret file mode is looser than 0600",
			"path", path, "mode", fmt.Sprintf("%#o", mode))
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("secretref: read %s: %w", path, err)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// schemeOf reports whether s starts with an upper-case scheme such as
// "KEYCHAIN:" and returns the scheme without the colon.
func schemeOf(s string) (string, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return "", false
	}
	for j, c := range s[:i] {
		switch {
		case c >= 'A' && c <= 'Z':
		case j > 0 && (c >= '0' && c <= '9' || c == '_'):
		default:
			return "", false
		}
	}
	return s[:i], true
}
