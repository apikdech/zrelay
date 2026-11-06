package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// NewRangeReaderAt adapts r into an io.ReaderAt over the object at key, whose
// size the caller already knows (from Stat or a sidecar). Each ReadAt issues
// one ranged OpenRange; reads are clamped to size and return io.EOF when they
// reach it, as io.ReaderAt requires. It is safe for concurrent use if r is.
func NewRangeReaderAt(ctx context.Context, r Reader, key string, size int64) io.ReaderAt {
	return &rangeReaderAt{ctx: ctx, r: r, key: key, size: size}
}

type rangeReaderAt struct {
	ctx  context.Context
	r    Reader
	key  string
	size int64
}

func (a *rangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("storage: ReadAt: negative offset")
	}
	if off >= a.size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), a.size-off)
	if want == 0 {
		return 0, nil
	}
	or, err := a.r.OpenRange(a.ctx, a.key, off, want)
	if err != nil {
		return 0, err
	}
	defer or.Close()
	n, err := io.ReadFull(or, p[:want])
	if err != nil {
		// The object is shorter than the size the caller was given.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = fmt.Errorf("storage: ReadAt %d bytes at %d: object shorter than %d: %w",
				want, off, a.size, io.ErrUnexpectedEOF)
		}
		return n, err
	}
	if want < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}
