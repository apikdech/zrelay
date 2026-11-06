// Package egress holds the memory-only upload path of the relay (relay-spec
// §4.8, design §6.3): a seekable ciphertext ring per clip and the backlog
// accountant that bounds all rings together.
package egress

import (
	"context"
	"errors"
	"io"
	"sync"
)

var (
	// ErrEvicted is returned once the backlog accountant evicted the stream.
	ErrEvicted = errors.New("egress: stream evicted")
	// ErrClosed is returned by Write after CloseWrite.
	ErrClosed = errors.New("egress: stream closed for writing")
	// ErrReleased is returned by ReadAt for bytes already released.
	ErrReleased = errors.New("egress: bytes already released")
)

// chunkSize is the allocation unit of the ring. Whole chunks below the
// released offset are dropped so their memory is reclaimed.
const chunkSize = 64 << 10

// Stream is the seekable ciphertext ring of one clip. One goroutine writes,
// one drain worker reads. Write never blocks. Bytes stay readable until
// Release passes them (the resume contract: the ring retains bytes until the
// driver's DurableOffset has passed them).
type Stream struct {
	mu       sync.Mutex
	chunks   [][]byte // chunks[0] holds offsets starting at first*chunkSize
	first    int64    // index of chunks[0]
	end      int64    // total bytes written
	released int64    // bytes below this offset are gone
	closed   bool
	evicted  bool
	notify   chan struct{} // closed and replaced on every state change
	evictCh  chan struct{}
	onGrow   func() // set by Accountant.Register; called without mu held
}

// NewStream returns an empty stream.
func NewStream() *Stream {
	return &Stream{notify: make(chan struct{}), evictCh: make(chan struct{})}
}

// wake must be called with mu held.
func (s *Stream) wake() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// Write appends p. It never blocks. After eviction it returns 0 and
// ErrEvicted; after CloseWrite, ErrClosed.
func (s *Stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.evicted {
		s.mu.Unlock()
		return 0, ErrEvicted
	}
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	if len(p) == 0 {
		s.mu.Unlock()
		return 0, nil
	}
	rest := p
	for len(rest) > 0 {
		idx := int(s.end/chunkSize - s.first)
		off := int(s.end % chunkSize)
		if idx == len(s.chunks) {
			s.chunks = append(s.chunks, make([]byte, chunkSize))
		}
		n := copy(s.chunks[idx][off:], rest)
		rest = rest[n:]
		s.end += int64(n)
	}
	s.wake()
	grow := s.onGrow
	s.mu.Unlock()
	if grow != nil {
		grow()
	}
	return len(p), nil
}

// CloseWrite marks the writer finished. It is idempotent.
func (s *Stream) CloseWrite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.wake()
}

// ReadAt copies up to len(p) bytes starting at off without blocking. It
// returns 0, nil when off is at the end of what has been written and the
// writer is not finished (use Wait). io.EOF is returned only once CloseWrite
// has been called and the read reaches the end.
func (s *Stream) ReadAt(off int64, p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evicted {
		return 0, ErrEvicted
	}
	if off < s.released {
		return 0, ErrReleased
	}
	if off > s.end {
		off = s.end
	}
	n := 0
	for n < len(p) && off < s.end {
		idx := int(off/chunkSize - s.first)
		in := int(off % chunkSize)
		avail := chunkSize - in
		if rem := s.end - off; rem < int64(avail) {
			avail = int(rem)
		}
		c := copy(p[n:], s.chunks[idx][in:in+avail])
		n += c
		off += int64(c)
	}
	if s.closed && off == s.end {
		return n, io.EOF
	}
	return n, nil
}

// Wait blocks until bytes exist beyond off, the writer finished, the stream
// was evicted (ErrEvicted) or ctx ended (ctx.Err()).
func (s *Stream) Wait(ctx context.Context, off int64) error {
	for {
		s.mu.Lock()
		switch {
		case s.evicted:
			s.mu.Unlock()
			return ErrEvicted
		case s.end > off || s.closed:
			s.mu.Unlock()
			return nil
		}
		ch := s.notify
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release drops bytes below upTo (the driver's DurableOffset). It never moves
// backwards and is clamped to the bytes written.
func (s *Stream) Release(upTo int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evicted {
		return
	}
	if upTo > s.end {
		upTo = s.end
	}
	if upTo <= s.released {
		return
	}
	s.released = upTo
	// Drop whole chunks that lie entirely below the released offset.
	for len(s.chunks) > 0 && (s.first+1)*chunkSize <= s.released {
		s.chunks[0] = nil
		s.chunks = s.chunks[1:]
		s.first++
	}
}

func (s *Stream) isEvicted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evicted
}

// Buffered returns the bytes written but not yet released.
func (s *Stream) Buffered() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end - s.released
}

// Evicted returns a channel closed when the stream is evicted. The owning
// drain worker watches it and aborts its ObjectWriter itself.
func (s *Stream) Evicted() <-chan struct{} { return s.evictCh }

// Evict discards every buffered byte and makes further Write, ReadAt and
// Wait fail with ErrEvicted. It is idempotent and reports whether this call
// performed the eviction.
func (s *Stream) Evict() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evicted {
		return false
	}
	s.evicted = true
	s.chunks = nil
	s.released = s.end
	close(s.evictCh)
	s.wake()
	return true
}
