package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"testing"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// memReader is a minimal in-memory storage.Reader holding one object.
type memReader struct {
	key   string
	data  []byte
	calls int
}

func (m *memReader) List(context.Context, string) iter.Seq2[storage.ObjectInfo, error] {
	return func(func(storage.ObjectInfo, error) bool) {}
}

func (m *memReader) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	if key != m.key {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: key, Size: int64(len(m.data))}, nil
}

func (m *memReader) OpenRange(_ context.Context, key string, off, n int64) (*storage.ObjectRange, error) {
	m.calls++
	if key != m.key {
		return nil, storage.ErrNotFound
	}
	end := int64(len(m.data))
	if n >= 0 && off+n < end {
		end = off + n
	}
	return &storage.ObjectRange{
		ReadCloser: io.NopCloser(bytes.NewReader(m.data[off:end])),
		Total:      int64(len(m.data)),
		ETag:       "x",
	}, nil
}

func TestRangeReaderAt(t *testing.T) {
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	m := &memReader{key: "clips/a.zrec", data: data}
	ra := storage.NewRangeReaderAt(context.Background(), m, m.key, int64(len(data)))

	buf := make([]byte, 100)
	n, err := ra.ReadAt(buf, 250)
	if err != nil || n != 100 || !bytes.Equal(buf, data[250:350]) {
		t.Fatalf("ReadAt(100@250) = %d, %v; bytes match = %v", n, err, bytes.Equal(buf[:n], data[250:350]))
	}

	// A read crossing the end returns the available bytes and io.EOF.
	n, err = ra.ReadAt(buf, 950)
	if n != 50 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:n], data[950:]) {
		t.Fatalf("ReadAt(100@950) = %d, %v; want 50, io.EOF", n, err)
	}

	// At or past the end: no request, io.EOF.
	before := m.calls
	n, err = ra.ReadAt(buf, 1000)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt(@1000) = %d, %v; want 0, io.EOF", n, err)
	}
	if m.calls != before {
		t.Fatalf("ReadAt at EOF issued %d OpenRange calls, want 0", m.calls-before)
	}

	if _, err := ra.ReadAt(buf, -1); err == nil {
		t.Fatal("ReadAt(negative offset) error = nil, want error")
	}

	// io.SectionReader over the adapter reads the whole object.
	all, err := io.ReadAll(io.NewSectionReader(ra, 0, int64(len(data))))
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("SectionReader ReadAll: err = %v, equal = %v", err, bytes.Equal(all, data))
	}
}

func TestRangeReaderAtShortObject(t *testing.T) {
	// The object is shorter than the size the caller was told: the adapter must
	// not pretend the missing bytes were read.
	m := &memReader{key: "k", data: []byte("abc")}
	ra := storage.NewRangeReaderAt(context.Background(), m, "k", 10)
	buf := make([]byte, 5)
	n, err := ra.ReadAt(buf, 0)
	if n != 3 || err == nil {
		t.Fatalf("ReadAt on short object = %d, %v; want 3 and an error", n, err)
	}
}
