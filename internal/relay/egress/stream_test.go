package egress_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/egress"
)

func TestStreamWriteNeverBlocks(t *testing.T) {
	s := egress.NewStream()
	done := make(chan struct{})
	go func() {
		defer close(done)
		chunk := make([]byte, 100_000)
		for i := 0; i < 50; i++ { // 5 MB, nobody reading
			n, err := s.Write(chunk)
			if err != nil || n != len(chunk) {
				t.Errorf("Write = %d, %v", n, err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Write blocked with no reader")
	}
	if got := s.Buffered(); got != 5_000_000 {
		t.Fatalf("Buffered = %d, want 5000000", got)
	}
}

func TestStreamRetainsUntilRelease(t *testing.T) {
	s := egress.NewStream()
	first := bytes.Repeat([]byte("a"), 100_000)
	second := bytes.Repeat([]byte("b"), 100_000)
	s.Write(first)
	buf := make([]byte, 10)
	if n, err := s.ReadAt(0, buf); n != 10 || err != nil {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	s.Write(second)
	// Reading from an offset below the later write still works.
	buf = make([]byte, 20)
	n, err := s.ReadAt(99_990, buf)
	if n != 20 || err != nil {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if string(buf) != "aaaaaaaaaabbbbbbbbbb" {
		t.Fatalf("ReadAt spanning = %q", buf)
	}
	// Re-reading from 0 works too: nothing released yet.
	if n, _ := s.ReadAt(0, buf[:5]); n != 5 {
		t.Fatalf("re-read = %d", n)
	}

	s.Release(150_000)
	if got := s.Buffered(); got != 50_000 {
		t.Fatalf("Buffered after Release = %d, want 50000", got)
	}
	if _, err := s.ReadAt(0, buf); !errors.Is(err, egress.ErrReleased) {
		t.Fatalf("ReadAt below release = %v, want ErrReleased", err)
	}
	if n, err := s.ReadAt(150_000, buf); n != 20 || err != nil || string(buf[:5]) != "bbbbb" {
		t.Fatalf("ReadAt at release = %d, %v", n, err)
	}
	s.Release(10) // never goes backwards
	if got := s.Buffered(); got != 50_000 {
		t.Fatalf("Release regressed: Buffered = %d", got)
	}
	s.Release(1 << 40) // clamped to written
	if got := s.Buffered(); got != 0 {
		t.Fatalf("Buffered = %d, want 0", got)
	}
}

func TestStreamEOFOnlyAfterCloseWrite(t *testing.T) {
	s := egress.NewStream()
	s.Write([]byte("hello"))
	buf := make([]byte, 16)
	n, err := s.ReadAt(0, buf)
	if n != 5 || err != nil {
		t.Fatalf("before close ReadAt = %d, %v; want 5, nil", n, err)
	}
	if n, err := s.ReadAt(5, buf); n != 0 || err != nil {
		t.Fatalf("at end before close = %d, %v; want 0, nil", n, err)
	}
	s.CloseWrite()
	n, err = s.ReadAt(0, buf)
	if n != 5 || (err != nil && err != io.EOF) {
		t.Fatalf("after close ReadAt = %d, %v", n, err)
	}
	if n, err := s.ReadAt(5, buf); n != 0 || err != io.EOF {
		t.Fatalf("at end after close = %d, %v; want 0, EOF", n, err)
	}
	if _, err := s.Write([]byte("x")); !errors.Is(err, egress.ErrClosed) {
		t.Fatalf("Write after CloseWrite = %v, want ErrClosed", err)
	}
	s.CloseWrite() // idempotent
}

func TestStreamWaitWakesOnWriteAndClose(t *testing.T) {
	s := egress.NewStream()
	errc := make(chan error, 1)
	go func() { errc <- s.Wait(context.Background(), 0) }()
	s.Write([]byte("x"))
	if err := <-errc; err != nil {
		t.Fatalf("Wait after write = %v", err)
	}
	go func() { errc <- s.Wait(context.Background(), 1) }()
	s.CloseWrite()
	if err := <-errc; err != nil {
		t.Fatalf("Wait after close = %v", err)
	}
}

func TestStreamWaitContextCancel(t *testing.T) {
	s := egress.NewStream()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Wait(ctx, 0) }()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
}

func TestEvictedStreamWriteErrors(t *testing.T) {
	s := egress.NewStream()
	s.Write([]byte("data"))
	waitErr := make(chan error, 1)
	go func() { waitErr <- s.Wait(context.Background(), 4) }()

	s.Evict()

	select {
	case <-s.Evicted():
	default:
		t.Fatal("Evicted channel not closed")
	}
	if n, err := s.Write([]byte("more")); n != 0 || !errors.Is(err, egress.ErrEvicted) {
		t.Fatalf("Write after evict = %d, %v; want 0, ErrEvicted", n, err)
	}
	if err := <-waitErr; !errors.Is(err, egress.ErrEvicted) {
		t.Fatalf("Wait = %v, want ErrEvicted", err)
	}
	if _, err := s.ReadAt(0, make([]byte, 4)); !errors.Is(err, egress.ErrEvicted) {
		t.Fatalf("ReadAt = %v, want ErrEvicted", err)
	}
	if got := s.Buffered(); got != 0 {
		t.Fatalf("Buffered after evict = %d, want 0", got)
	}
	s.Evict() // idempotent
}
