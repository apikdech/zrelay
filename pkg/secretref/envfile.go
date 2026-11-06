package secretref

import (
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile loads KEY=VALUE lines from path into the process environment
// (relay-spec §5.1 --env-file). Variables already set in the environment
// win and are left unchanged. Blank lines and lines starting with '#' are
// skipped, an optional "export " prefix is accepted, and one pair of
// matching single or double quotes around a value is stripped.
//
// The whole file is parsed before anything is set, so a malformed file
// changes nothing. Parse errors name the line number, never its content.
func LoadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("secretref: env file: %w", err)
	}
	type kv struct{ k, v string }
	var pairs []kv
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return fmt.Errorf("secretref: env file %s: line %d is not KEY=VALUE", path, i+1)
		}
		pairs = append(pairs, kv{k, unquote(strings.TrimSpace(v))})
	}
	for _, p := range pairs {
		if _, set := os.LookupEnv(p.k); set {
			continue
		}
		if err := os.Setenv(p.k, p.v); err != nil {
			return fmt.Errorf("secretref: env file %s: set %s: %w", path, p.k, err)
		}
	}
	return nil
}

// unquote strips one pair of matching single or double quotes.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
