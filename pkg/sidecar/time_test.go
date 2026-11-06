package sidecar

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTimeMarshalMillis(t *testing.T) {
	wib := time.FixedZone("WIB", 7*3600)
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 9, 16, 14, 59, 0, 0, time.UTC), `"2026-09-16T14:59:00.000Z"`},
		{time.Date(2026, 9, 16, 14, 59, 42, 500e6, time.UTC), `"2026-09-16T14:59:42.500Z"`},
		{time.Date(2026, 9, 16, 14, 59, 42, 123456789, time.UTC), `"2026-09-16T14:59:42.123Z"`},
		{time.Date(2026, 9, 16, 21, 59, 0, 7e6, wib), `"2026-09-16T14:59:00.007Z"`},
	}
	for _, c := range cases {
		b, err := json.Marshal(Time{c.in})
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("marshal %v = %s, want %s", c.in, b, c.want)
		}
	}

	// Any precision and any offset on unmarshal; the result is UTC.
	for in, want := range map[string]time.Time{
		`"2026-09-16T15:00:00Z"`:                time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC),
		`"2026-09-16T15:00:00.5Z"`:              time.Date(2026, 9, 16, 15, 0, 0, 500e6, time.UTC),
		`"2026-09-16T15:00:00.123456789Z"`:      time.Date(2026, 9, 16, 15, 0, 0, 123456789, time.UTC),
		`"2026-09-16T22:00:00.250+07:00"`:       time.Date(2026, 9, 16, 15, 0, 0, 250e6, time.UTC),
	} {
		var got Time
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", in, err)
		}
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("unmarshal %s = %v, want %v UTC", in, got.Time, want)
		}
	}

	for _, bad := range []string{`"2026-09-16 15:00:00"`, `"yesterday"`, `12345`} {
		var got Time
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("unmarshal %s: accepted", bad)
		}
	}
}
