package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/zview"
)

// TestOpenStorageS3GivesUpOnSilentServer: a MinIO that accepts the connection
// and then goes silent (hung process, half-open tailnet path) must make a
// request fail instead of hanging forever, or every day-index load, and
// every request queued behind it, would hang with it.
func TestOpenStorageS3GivesUpOnSilentServer(t *testing.T) {
	old := s3ResponseTimeout
	s3ResponseTimeout = 300 * time.Millisecond
	t.Cleanup(func() { s3ResponseTimeout = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }() // read, never answer
		}
	}()

	cfg := &zview.Config{}
	cfg.Storage.Driver = "s3"
	cfg.Storage.Root = "zrelay"
	cfg.Storage.S3 = zview.S3Config{
		Endpoint: "http://" + ln.Addr().String(), Region: "us-east-1", Bucket: "b",
		AccessKey: "ak", SecretKey: "sk", UsePathStyle: true,
	}
	store, err := openStorage(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := store.Stat(ctx, "status/relay.json"); err == nil {
		t.Fatal("Stat succeeded against a silent server")
	}
	if ctx.Err() != nil {
		t.Fatalf("Stat hung until the test deadline (%v): the HTTP client has no response timeout", time.Since(start))
	}
}
