// Package storage defines the provider interfaces shared by zrelay (writer)
// and zview-server (reader), relay-spec §4.9. Keys are relative to the storage
// root (Contract §7.1) and always pass through CleanKey.
package storage

import (
	"context"
	"errors"
	"io"
	"iter"
	"time"
)

// MaxSmallSize is the largest object PutSmall accepts (1 MiB).
const MaxSmallSize = 1 << 20

var (
	// ErrNotFound reports that no object exists at the key.
	ErrNotFound = errors.New("storage: object not found")
	// ErrInvalidKey reports a key or prefix that CleanKey rejects.
	ErrInvalidKey = errors.New("storage: invalid key")
	// ErrNothingToRecover reports that a recovery candidate held no durable
	// bytes; the driver discarded it instead of publishing an empty object.
	ErrNothingToRecover = errors.New("storage: nothing to recover")
)

// ErrorClass is how egress treats a failed storage request (design §6.3).
type ErrorClass int

const (
	// Transient errors (network errors, 5xx, 429, RequestTimeout, SlowDown)
	// are retried and count toward opening the circuit breaker.
	Transient ErrorClass = iota
	// Permanent errors (403, NoSuchBucket, InvalidAccessKeyId,
	// SignatureDoesNotMatch, other 4xx) open the breaker at once.
	Permanent
	// NotFound reports a missing object (or upload).
	NotFound
)

type ObjectInfo struct {
	Key     string
	Size    int64
	ModTime time.Time
	ETag    string
}

type ObjectWriter interface {
	io.Writer // may block (backpressure)

	// DurableOffset reports how many bytes the driver has accepted and will
	// not ask for again. During streaming this means accepted by the
	// operating system (or, for s3, in a completed part) -- NOT fsynced.
	// The durability barrier is Commit. This is what the documented kill -9
	// recovery (Contract §7.4) needs, and it avoids an fsync per chunk on a
	// high-latency network mount.
	DurableOffset() int64

	// Resume reconnects if needed, repositions the driver to a known-good
	// offset, and returns it. The caller MUST continue writing from exactly
	// that offset. io.Writer has no rewind, so without this the driver and
	// the caller can silently disagree about where the next byte belongs and
	// the committed object ends up with duplicated or missing bytes.
	//
	// The returned offset is always >= the last DurableOffset: that value is
	// a monotonic floor the backlog ring relies on to release bytes (§4.8).
	Resume(ctx context.Context) (int64, error)

	// Commit atomically publishes the object (rename / CompleteMultipartUpload)
	// and is the durability barrier: filesystem drivers fsync the staging file
	// and then the directory chain from the object's directory up to the storage
	// root before returning (a new directory's entry is durable only once its own
	// parent is fsynced).
	Commit(ctx context.Context) error

	Abort(ctx context.Context) error
}

type Writer interface {
	Create(ctx context.Context, key string) (ObjectWriter, error)
	PutSmall(ctx context.Context, key string, data []byte, contentType string) error // atomic, ≤ 1 MiB
	Delete(ctx context.Context, key string) error
}

// ObjectRange carries the whole-object metadata a Range response needs, so callers
// do not have to pair every OpenRange with a Stat. Objects are immutable once
// committed (Contract §7.3), so the values cannot go stale mid-read.
type ObjectRange struct {
	io.ReadCloser
	Total int64 // full object size, not the length of this range
	ETag  string
}

type Reader interface {
	// List yields at most one non-nil error and then stops. Prefix matching is
	// recursive and by whole path element: an object matches iff its key equals
	// prefix or begins with prefix + "/", so List("clips/front") does NOT return
	// clips/front-door/... (an S3 driver must filter its byte-prefix results to
	// match). An empty prefix lists everything; a prefix naming nothing yields an
	// empty sequence, not an error; a malformed prefix yields ErrInvalidKey. Order
	// is unspecified. Entries whose base name begins with "." are omitted
	// (Contract §7.2); callers that break early are safe.
	List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	OpenRange(ctx context.Context, key string, off, n int64) (*ObjectRange, error) // n = -1 → to EOF
}

// Incomplete describes one crash-recovery candidate (Contract §7.4).
type Incomplete struct {
	Key        string // final key the object takes once committed
	StagingKey string // ".<name>.part"; empty for s3
	UploadID   string // s3 multipart upload id; empty otherwise
	Size       int64  // best-effort durable bytes (may be 0); never the intended length
	ModTime    time.Time
}

type Recoverer interface { // optional; used at startup (Contract §7.4)
	ListIncomplete(ctx context.Context, prefix string, olderThan time.Duration) ([]Incomplete, error)
	RecoverCommit(ctx context.Context, inc Incomplete) (ObjectInfo, error)

	// ListStaging returns the staging sidecars ".<name>.json.part" (Contract
	// §7.3 step 0) under prefix, which List hides because they are dot-names.
	// Prefix matching is by whole path element, as List; an empty prefix lists
	// everything and a malformed one yields ErrInvalidKey. Keys are full object
	// keys including the dot-name. Order is unspecified.
	ListStaging(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// ReadProvider is what zview-server binds to, so a data-mode deployment
// with read-only credentials cannot be handed write methods at all.
type ReadProvider interface {
	Reader
	Name() string
	HealthCheck(ctx context.Context) error
	Close() error
}

type Provider interface {
	ReadProvider
	Writer
}
