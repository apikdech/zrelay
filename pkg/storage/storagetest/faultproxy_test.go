package storagetest

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer answers every request with the number of body bytes it read.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received.Add(n)
		_, _ = io.WriteString(w, strconv.FormatInt(n, 10))
	}))
	t.Cleanup(srv.Close)
	return srv, &received
}

func post(c *http.Client, url string, n int) (string, error) {
	resp, err := c.Post(url, "application/octet-stream", bytes.NewReader(make([]byte, n)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
	}
}

func TestFaultProxyForwards(t *testing.T) {
	srv, _ := countingServer(t)
	p := StartFaultProxy(t, srv.URL)
	c := newClient(10 * time.Second)
	for range 3 {
		got, err := post(c, p.URL(), 100_000)
		if err != nil {
			t.Fatalf("POST through proxy: %v", err)
		}
		if got != "100000" {
			t.Fatalf("upstream read %s bytes, want 100000", got)
		}
	}
}

func TestFaultProxyKillAfterBytes(t *testing.T) {
	srv, received := countingServer(t)
	p := StartFaultProxy(t, srv.URL)
	c := newClient(10 * time.Second)

	// Warm a keep-alive connection: the cut must apply to it too.
	if _, err := post(c, p.URL(), 10); err != nil {
		t.Fatalf("warm-up POST: %v", err)
	}
	received.Store(0)

	p.KillAfterBytes(200_000)
	if _, err := post(c, p.URL(), 1_000_000); err == nil {
		t.Fatal("POST of 1 MB with a 200 kB budget succeeded, want a cut connection")
	}
	if got := received.Load(); got >= 200_000 {
		t.Errorf("upstream received %d body bytes, want < 200000 (budget includes headers)", got)
	}
	// One-shot: the next request goes through.
	got, err := post(c, p.URL(), 1_000_000)
	if err != nil {
		t.Fatalf("POST after the cut: %v", err)
	}
	if got != "1000000" {
		t.Fatalf("upstream read %s bytes after the cut, want 1000000", got)
	}
}

func TestFaultProxyBlackhole(t *testing.T) {
	srv, _ := countingServer(t)
	p := StartFaultProxy(t, srv.URL)

	p.Blackhole(true)
	if _, err := post(newClient(300*time.Millisecond), p.URL(), 10); err == nil {
		t.Fatal("POST through a blackholed proxy succeeded, want a timeout")
	}
	p.Blackhole(false)
	got, err := post(newClient(10*time.Second), p.URL(), 10)
	if err != nil {
		t.Fatalf("POST after the blackhole lifted: %v", err)
	}
	if got != "10" {
		t.Fatalf("upstream read %s bytes, want 10", got)
	}
}
