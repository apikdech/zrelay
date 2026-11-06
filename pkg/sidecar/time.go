package sidecar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// millisLayout is RFC 3339 in UTC with exactly three fractional digits.
const millisLayout = "2006-01-02T15:04:05.000Z"

// Time is a wall time serialised as an RFC 3339 UTC string. Marshal always
// writes exactly three fractional digits (milliseconds, truncated);
// unmarshal accepts any precision and offset and normalises to UTC
// (Contract D9).
type Time struct {
	time.Time
}

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.UTC().Format(millisLayout) + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler. A JSON null leaves t unchanged.
func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("sidecar: time must be an RFC 3339 string: %w", err)
	}
	p, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("sidecar: invalid RFC 3339 time %q: %w", s, err)
	}
	t.Time = p.UTC()
	return nil
}
