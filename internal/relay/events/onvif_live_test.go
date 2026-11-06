package events_test

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

// TestLivePullPoint follows a real camera's ONVIF events with the built-in
// Tapo rules. It is a manual check (docs/LIVE-CHECKS.md, Task 8) and skips
// unless ZRELAY_LIVE_ONVIF_HOST is set. Credentials come from
// ZRELAY_LIVE_ONVIF_USER and ZRELAY_LIVE_ONVIF_PASSWORD and are never
// logged.
func TestLivePullPoint(t *testing.T) {
	host := os.Getenv("ZRELAY_LIVE_ONVIF_HOST")
	if host == "" {
		t.Skip("ZRELAY_LIVE_ONVIF_HOST not set")
	}
	port := envInt(t, "ZRELAY_LIVE_ONVIF_PORT", 2020)
	seconds := envInt(t, "ZRELAY_LIVE_ONVIF_SECONDS", 150)

	m := &countingMetrics{}
	pp := &events.PullPoint{
		Camera:   "live",
		Host:     host,
		Port:     port,
		Username: os.Getenv("ZRELAY_LIVE_ONVIF_USER"),
		Password: os.Getenv("ZRELAY_LIVE_ONVIF_PASSWORD"),
		Rules:    events.TapoRules(),
		Clock:    clock.Real,
		Metrics:  m,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	var mu sync.Mutex
	var ready []bool
	err := pp.Run(ctx, func(s events.Signal) {
		t.Logf("signal kind=%s active=%v initialized=%v topic=%s at=%s", s.Kind, s.Active, s.Initialized, s.RawTopic, s.At.Format(time.RFC3339))
	}, func(r bool) {
		mu.Lock()
		ready = append(ready, r)
		mu.Unlock()
		t.Logf("ready=%v", r)
	})
	t.Logf("Run returned %v", err)

	m.mu.Lock()
	t.Logf("pull retries=%d resubscribes=%d clock skew=%v unmatched=%v", m.pullRetries, m.resubscribe, m.skew, m.unmatched)
	m.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	// The subscription must come up once and survive every Renew; the final
	// false is the shutdown.
	if len(ready) != 2 || !ready[0] || ready[1] {
		t.Fatalf("ready calls = %v, want [true false] (one subscription kept alive by Renew)", ready)
	}
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return n
}
