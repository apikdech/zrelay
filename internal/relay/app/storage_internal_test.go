package app

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/config"
)

// TestDefaultStorageS3GivesUpOnSilentServer: a storage endpoint that accepts
// the connection and then goes silent (a network partition) must make a
// request fail instead of hanging forever, or the circuit breaker, which
// opens on failures, would never open and every drain worker would hang.
func TestDefaultStorageS3GivesUpOnSilentServer(t *testing.T) {
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

	cfg := &config.Config{}
	cfg.Storage.Driver = "s3"
	cfg.Storage.Root = "zrelay"
	cfg.Storage.S3 = config.S3{
		Endpoint: "http://" + ln.Addr().String(), Region: "us-east-1", Bucket: "b",
		AccessKey: "ak", SecretKey: "sk", UsePathStyle: true, PartSizeMiB: 8,
	}
	prov, _, err := DefaultStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err = prov.HealthCheck(ctx)
	if err == nil {
		t.Fatal("HealthCheck succeeded against a silent server")
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		t.Fatalf("HealthCheck hung until the test deadline (%v): the HTTP client has no response timeout", time.Since(start))
	}
}
