package s3_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"

	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
)

func TestClassify(t *testing.T) {
	// Real SDK errors, produced by the driver against a stub answering with
	// the given status and S3 error code.
	api := []struct {
		status int
		code   string
		want   storage.ErrorClass
	}{
		{http.StatusServiceUnavailable, "ServiceUnavailable", storage.Transient},
		{http.StatusServiceUnavailable, "SlowDown", storage.Transient},
		{http.StatusInternalServerError, "InternalError", storage.Transient},
		{http.StatusBadRequest, "RequestTimeout", storage.Transient},
		{http.StatusTooManyRequests, "TooManyRequests", storage.Transient},
		{http.StatusForbidden, "AccessDenied", storage.Permanent},
		{http.StatusNotFound, "NoSuchBucket", storage.Permanent},
		{http.StatusForbidden, "InvalidAccessKeyId", storage.Permanent},
		{http.StatusForbidden, "SignatureDoesNotMatch", storage.Permanent},
		{http.StatusNotFound, "NoSuchKey", storage.NotFound},
	}
	for _, c := range api {
		t.Run(fmt.Sprintf("%d_%s", c.status, c.code), func(t *testing.T) {
			f := newFakeS3(t)
			f.failWith(c.status, c.code)
			d := stubDriver(t, f.URL, "cctv")
			err := d.PutSmall(t.Context(), "status/relay.json", []byte("{}"), "application/json")
			if err == nil {
				t.Fatal("PutSmall error = nil, want error")
			}
			if got := s3.Classify(err); got != c.want {
				t.Errorf("Classify(%v) = %d, want %d", err, got, c.want)
			}
		})
	}

	// HEAD responses carry no body: the class comes from the status alone.
	heads := []struct {
		name   string
		status int
		want   storage.ErrorClass
	}{
		{"HeadBucket503", http.StatusServiceUnavailable, storage.Transient},
		{"HeadBucket403", http.StatusForbidden, storage.Permanent},
		{"HeadBucket404IsNoSuchBucket", http.StatusNotFound, storage.Permanent},
	}
	for _, c := range heads {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeS3(t)
			f.failWith(c.status, "")
			d := stubDriver(t, f.URL, "cctv")
			err := d.HealthCheck(t.Context())
			if err == nil {
				t.Fatal("HealthCheck error = nil, want error")
			}
			if got := s3.Classify(err); got != c.want {
				t.Errorf("Classify(%v) = %d, want %d", err, got, c.want)
			}
		})
	}

	t.Run("ConnectionRefused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		d := stubDriver(t, "http://"+addr, "")
		err = d.HealthCheck(t.Context())
		if err == nil {
			t.Fatal("HealthCheck against a closed port error = nil, want error")
		}
		if got := s3.Classify(err); got != storage.Transient {
			t.Errorf("Classify(%v) = %d, want Transient", err, got)
		}
	})

	plain := []struct {
		name string
		err  error
		want storage.ErrorClass
	}{
		{"NetOpError", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, storage.Transient},
		{"WrappedNetOpError", fmt.Errorf("s3: write: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), storage.Transient},
		{"DeadlineExceeded", fmt.Errorf("s3: %w", context.DeadlineExceeded), storage.Transient},
		{"ErrNotFound", fmt.Errorf("s3: stat: %w", storage.ErrNotFound), storage.NotFound},
		{"ErrInvalidKey", fmt.Errorf("x: %w", storage.ErrInvalidKey), storage.Permanent},
		{"Unknown", errors.New("s3: writer is closed"), storage.Permanent},
	}
	for _, c := range plain {
		t.Run(c.name, func(t *testing.T) {
			if got := s3.Classify(c.err); got != c.want {
				t.Errorf("Classify(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}
