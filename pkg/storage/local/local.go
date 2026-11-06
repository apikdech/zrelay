//go:build unix

// Package local is the filesystem storage driver (relay-spec §4.9). It is
// intended for tests: it is the only driver that touches the host's
// filesystem, and production zrelay runs with a read-only root and no volumes.
//
// Objects live at <base_dir>/<root>/<key> (Contract §7.1). Streamed objects are
// staged as ".<name>.part" and renamed into place by Commit; PutSmall stages
// through ".<name>.tmp" and renames, never through ".part" (Contract §7.2).
// Commit and PutSmall fsync the file and then every directory from the
// object's directory up to and including base_dir.
//
// A filesystem has no entity tag, so ETag is synthesized from size, mtime (ns)
// and inode: overwrites publish by rename, which installs a fresh inode while
// the old one is still alive, so the tag changes even on coarse-mtime mounts.
// Supported on unix only.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

const (
	stagingSuffix = ".part"
	tmpSuffix     = ".tmp"
	dirPerm       = 0o750
	filePerm      = 0o640
)

// Driver is the local filesystem storage.Provider and storage.Recoverer.
type Driver struct {
	base string // absolute base_dir (the mount anchor)
	dir  string // base joined with the storage root

	putMu sync.Mutex // serializes PutSmall so ".<name>.tmp" is never shared
}

var (
	_ storage.Provider  = (*Driver)(nil)
	_ storage.Recoverer = (*Driver)(nil)
)

// New returns a driver rooted at baseDir/root. baseDir must be an existing
// directory. root is the storage.root key prefix (may be empty); per Contract
// §7.1 baseDir must not already include it, so a baseDir whose last element
// equals root's first element is rejected.
func New(baseDir, root string) (*Driver, error) {
	if baseDir == "" {
		return nil, errors.New("local: base_dir is empty")
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("local: base_dir: %w", err)
	}
	fi, err := os.Stat(base)
	if err != nil {
		return nil, fmt.Errorf("local: base_dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("local: base_dir %q is not a directory", base)
	}
	dir := base
	if root != "" {
		if _, err := storage.CleanKey(root); err != nil {
			return nil, fmt.Errorf("local: root: %w", err)
		}
		first, _, _ := strings.Cut(root, "/")
		if filepath.Base(base) == first {
			return nil, fmt.Errorf("local: base_dir %q already ends in root element %q (Contract §7.1)", base, first)
		}
		dir = filepath.Join(base, filepath.FromSlash(root))
	}
	return &Driver{base: base, dir: dir}, nil
}

// Name implements storage.ReadProvider.
func (d *Driver) Name() string { return "local" }

// HealthCheck reports whether base_dir is still a reachable directory.
func (d *Driver) HealthCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fi, err := os.Stat(d.base)
	if err != nil {
		return fmt.Errorf("local: health: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("local: health: %q is not a directory", d.base)
	}
	return nil
}

// Close implements storage.ReadProvider. The driver holds no resources.
func (d *Driver) Close() error { return nil }

// path maps a clean key (or "" for the root) to its filesystem path.
func (d *Driver) path(key string) string {
	if key == "" {
		return d.dir
	}
	return filepath.Join(d.dir, filepath.FromSlash(key))
}

// key maps a filesystem path under d.dir back to its object key.
func (d *Driver) key(p string) (string, error) {
	rel, err := filepath.Rel(d.dir, p)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func cleanPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	_, err := storage.CleanKey(prefix)
	return err
}

func notFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func etag(fi fs.FileInfo) string {
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return fmt.Sprintf("%x-%x-%x", fi.Size(), fi.ModTime().UnixNano(), ino)
}

func objectInfo(key string, fi fs.FileInfo) storage.ObjectInfo {
	return storage.ObjectInfo{
		Key:     key,
		Size:    fi.Size(),
		ModTime: fi.ModTime().UTC(),
		ETag:    etag(fi),
	}
}

// Stat implements storage.Reader. Dot-names are not filtered.
func (d *Driver) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if _, err := storage.CleanKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	fi, err := os.Stat(d.path(key))
	if notFound(err) || (err == nil && !fi.Mode().IsRegular()) {
		return storage.ObjectInfo{}, fmt.Errorf("local: stat %q: %w", key, storage.ErrNotFound)
	}
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("local: stat: %w", err)
	}
	return objectInfo(key, fi), nil
}

// OpenRange implements storage.Reader. A range extending past the end is
// clamped to the object; an offset beyond the end is an error.
func (d *Driver) OpenRange(ctx context.Context, key string, off, n int64) (*storage.ObjectRange, error) {
	if _, err := storage.CleanKey(key); err != nil {
		return nil, err
	}
	if off < 0 || n < -1 {
		return nil, fmt.Errorf("local: open %q: invalid range off=%d n=%d", key, off, n)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(d.path(key))
	if notFound(err) {
		return nil, fmt.Errorf("local: open %q: %w", key, storage.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("local: open: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("local: open: %w", err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("local: open %q: %w", key, storage.ErrNotFound)
	}
	size := fi.Size()
	if off > size {
		_ = f.Close()
		return nil, fmt.Errorf("local: open %q: offset %d beyond size %d", key, off, size)
	}
	length := size - off
	if n >= 0 && n < length {
		length = n
	}
	return &storage.ObjectRange{
		ReadCloser: struct {
			io.Reader
			io.Closer
		}{io.NewSectionReader(f, off, length), f},
		Total: size,
		ETag:  etag(fi),
	}, nil
}

// walk visits every regular file whose key equals prefix or lies under
// prefix + "/". fn returning false stops the walk early.
func (d *Driver) walk(ctx context.Context, prefix string, fn func(key string, fi fs.FileInfo) bool) error {
	start := d.path(prefix)
	fi, err := os.Lstat(start)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("local: list: %w", err)
	}
	if fi.Mode().IsRegular() {
		fn(prefix, fi)
		return nil
	}
	if !fi.IsDir() {
		return nil
	}
	errStop := errors.New("stop")
	err = filepath.WalkDir(start, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if notFound(err) { // removed while walking
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.Type().IsRegular() {
			return nil
		}
		info, err := e.Info()
		if notFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		key, err := d.key(p)
		if err != nil {
			return err
		}
		if !fn(key, info) {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errStop) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("local: list: %w", err)
	}
	return nil
}

// List implements storage.Reader. Entries whose base name begins with "."
// are omitted (Contract §7.2).
func (d *Driver) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		if err := cleanPrefix(prefix); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		if err := ctx.Err(); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		stopped := false
		err := d.walk(ctx, prefix, func(key string, fi fs.FileInfo) bool {
			if strings.HasPrefix(filepath.Base(key), ".") {
				return true
			}
			if !yield(objectInfo(key, fi), nil) {
				stopped = true
				return false
			}
			return true
		})
		if err != nil && !stopped {
			yield(storage.ObjectInfo{}, err)
		}
	}
}

// syncDirChain fsyncs dir and every parent up to and including base_dir, so
// that newly created directory entries are durable too.
func (d *Driver) syncDirChain(dir string) error {
	for p := dir; ; p = filepath.Dir(p) {
		if p != d.base && !strings.HasPrefix(p, d.base+string(filepath.Separator)) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("local: sync dir: %w", err)
		}
		err = f.Sync()
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("local: sync dir: %w", err)
		}
		if p == d.base {
			return nil
		}
	}
}

// stagedPath returns the sibling ".<name><suffix>" of p.
func stagedPath(p, suffix string) string {
	return filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+suffix)
}

// PutSmall implements storage.Writer: it writes ".<name>.tmp", fsyncs it,
// renames it over the final name and fsyncs the directory chain.
func (d *Driver) PutSmall(ctx context.Context, key string, data []byte, _ string) error {
	if _, err := storage.CleanKey(key); err != nil {
		return err
	}
	if len(data) > storage.MaxSmallSize {
		return fmt.Errorf("local: put %q: %d bytes exceeds the %d-byte limit", key, len(data), storage.MaxSmallSize)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.putMu.Lock()
	defer d.putMu.Unlock()

	final := d.path(key)
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return fmt.Errorf("local: put: %w", err)
	}
	tmp := stagedPath(final, tmpSuffix)
	if err := writeSynced(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("local: put: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("local: put: %w", err)
	}
	return d.syncDirChain(filepath.Dir(final))
}

func writeSynced(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Delete implements storage.Writer. Deleting a missing object is not an error.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if _, err := storage.CleanKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := d.path(key)
	fi, err := os.Lstat(p)
	if notFound(err) || (err == nil && fi.IsDir()) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("local: delete: %w", err)
	}
	if err := os.Remove(p); err != nil && !notFound(err) {
		return fmt.Errorf("local: delete: %w", err)
	}
	return nil
}

// Create implements storage.Writer, staging the object as ".<name>.part".
func (d *Driver) Create(ctx context.Context, key string) (storage.ObjectWriter, error) {
	if _, err := storage.CleanKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	final := d.path(key)
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return nil, fmt.Errorf("local: create: %w", err)
	}
	staging := stagedPath(final, stagingSuffix)
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return nil, fmt.Errorf("local: create: %w", err)
	}
	return &objectWriter{d: d, final: final, staging: staging, f: f}, nil
}

var errClosed = errors.New("local: writer is closed")

type objectWriter struct {
	d              *Driver
	final, staging string
	off            atomic.Int64 // bytes accepted by the OS

	mu        sync.Mutex
	f         *os.File
	closed    bool // committed, aborted or failed: no more writes
	committed bool
}

func (w *objectWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errClosed
	}
	n, err := w.f.Write(p)
	w.off.Add(int64(n))
	return n, err
}

func (w *objectWriter) DurableOffset() int64 { return w.off.Load() }

// Resume repositions to the staging file's size, which includes every byte the
// OS accepted and so is never below DurableOffset.
func (w *objectWriter) Resume(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errClosed
	}
	fi, err := w.f.Stat()
	if err != nil {
		return 0, fmt.Errorf("local: resume: %w", err)
	}
	size := fi.Size()
	if size < w.off.Load() {
		return 0, fmt.Errorf("local: resume: staging file shrank to %d below durable offset %d", size, w.off.Load())
	}
	if _, err := w.f.Seek(size, io.SeekStart); err != nil {
		return 0, fmt.Errorf("local: resume: %w", err)
	}
	w.off.Store(size)
	return size, nil
}

// Commit fsyncs the staging file, renames it into place and fsyncs the
// directory chain up to base_dir.
func (w *objectWriter) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errClosed
	}
	w.closed = true
	err := w.f.Sync()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.f = nil
	if err != nil {
		return fmt.Errorf("local: commit: %w", err)
	}
	if err := os.Rename(w.staging, w.final); err != nil {
		return fmt.Errorf("local: commit: %w", err)
	}
	w.committed = true
	return w.d.syncDirChain(filepath.Dir(w.final))
}

// Abort discards the staging file. It is a no-op once aborted and an error
// once committed.
func (w *objectWriter) Abort(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return errors.New("local: abort: already committed")
	}
	w.closed = true
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	if err := os.Remove(w.staging); err != nil && !notFound(err) {
		return fmt.Errorf("local: abort: %w", err)
	}
	return nil
}

// finalName returns X for a staging base name ".X.part", or "" when name is
// not a staging name for a valid key element.
func finalName(name string) string {
	if len(name) <= 1+len(stagingSuffix) || name[0] != '.' || !strings.HasSuffix(name, stagingSuffix) {
		return ""
	}
	x := name[1 : len(name)-len(stagingSuffix)]
	if x == "." || x == ".." {
		return ""
	}
	return x
}

// ListIncomplete implements storage.Recoverer: every ".<name>.part" file whose
// final key matches prefix by whole path element and whose mtime is at least
// olderThan ago. Final keys ending in ".json" are skipped: only ".zrec" video is
// streamed (Contract §7.3 step 1), and ".<name>.json.part" is the staging
// sidecar written by PutSmall (step 0), which s3 never lists either. Recovery
// promotes staging sidecars by its own rules (Contract §7.4).
func (d *Driver) ListIncomplete(ctx context.Context, prefix string, olderThan time.Duration) ([]storage.Incomplete, error) {
	if err := cleanPrefix(prefix); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-olderThan)
	var out []storage.Incomplete
	add := func(stagingKey string, fi fs.FileInfo) {
		dir, name := splitKey(stagingKey)
		x := finalName(name)
		if x == "" || strings.HasSuffix(x, ".json") || fi.ModTime().After(cutoff) {
			return
		}
		out = append(out, storage.Incomplete{
			Key:        joinKey(dir, x),
			StagingKey: stagingKey,
			Size:       fi.Size(),
			ModTime:    fi.ModTime().UTC(),
		})
	}
	err := d.walk(ctx, prefix, func(key string, fi fs.FileInfo) bool {
		add(key, fi)
		return true
	})
	if err != nil {
		return nil, err
	}
	// The prefix may itself name the final key of a staged object.
	if prefix != "" {
		dir, name := splitKey(prefix)
		sk := joinKey(dir, "."+name+stagingSuffix)
		if fi, err := os.Lstat(d.path(sk)); err == nil && fi.Mode().IsRegular() {
			add(sk, fi)
		}
	}
	return out, nil
}

func splitKey(key string) (dir, name string) {
	i := strings.LastIndexByte(key, '/')
	if i < 0 {
		return "", key
	}
	return key[:i], key[i+1:]
}

func joinKey(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// RecoverCommit implements storage.Recoverer: it fsyncs the staging file and
// renames it to its final key. An empty staging file is removed and
// ErrNothingToRecover returned. The caller must validate the descriptor first
// (Contract §7.4); this commits whatever bytes are there.
func (d *Driver) RecoverCommit(ctx context.Context, inc storage.Incomplete) (storage.ObjectInfo, error) {
	if _, err := storage.CleanKey(inc.StagingKey); err != nil {
		return storage.ObjectInfo{}, err
	}
	dir, name := splitKey(inc.StagingKey)
	x := finalName(name)
	if x == "" || joinKey(dir, x) != inc.Key {
		return storage.ObjectInfo{}, fmt.Errorf("%w: staging key %q does not stage %q", storage.ErrInvalidKey, inc.StagingKey, inc.Key)
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	staging := d.path(inc.StagingKey)
	f, err := os.OpenFile(staging, os.O_RDWR, 0)
	if notFound(err) {
		return storage.ObjectInfo{}, fmt.Errorf("local: recover %q: %w", inc.StagingKey, storage.ErrNotFound)
	}
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("local: recover: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return storage.ObjectInfo{}, fmt.Errorf("local: recover: %w", err)
	}
	if fi.Size() == 0 {
		_ = f.Close()
		if err := os.Remove(staging); err != nil && !notFound(err) {
			return storage.ObjectInfo{}, fmt.Errorf("local: recover: %w", err)
		}
		return storage.ObjectInfo{}, storage.ErrNothingToRecover
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("local: recover: %w", err)
	}
	final := d.path(inc.Key)
	if err := os.Rename(staging, final); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("local: recover: %w", err)
	}
	if err := d.syncDirChain(filepath.Dir(final)); err != nil {
		return storage.ObjectInfo{}, err
	}
	return d.Stat(ctx, inc.Key)
}
