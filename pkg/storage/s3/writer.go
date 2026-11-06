package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

var (
	errClosed      = errors.New("s3: writer is closed")
	errNeedsResume = errors.New("s3: writer needs Resume after a failed request")
	errCompleting  = errors.New("s3: writer refuses Write once CompleteMultipartUpload has been sent")
	// errUnpublished: the upload is gone (NoSuchUpload) and the object is
	// missing or has the wrong size, so no retry can publish it (Permanent).
	errUnpublished = errors.New("s3: upload is gone and the object was not published")
	// errAbortCommitted: Abort found the upload gone because an earlier
	// Commit's CompleteMultipartUpload published the object (Permanent).
	errAbortCommitted = errors.New("s3: abort: the object was already published by Commit")
)

// Create implements storage.Writer by starting a multipart upload. Part
// uploads inside Write run under ctx, so cancelling it interrupts a Write
// blocked on the network; Resume, Commit and Abort take their own contexts.
func (d *Driver) Create(ctx context.Context, key string) (storage.ObjectWriter, error) {
	if _, err := storage.CleanKey(key); err != nil {
		return nil, err
	}
	out, err := d.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.objectKey(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: create %q: %w", key, err)
	}
	return &objectWriter{d: d, ctx: ctx, key: key, uploadID: aws.ToString(out.UploadId)}, nil
}

// objectWriter buffers one part in memory and uploads it synchronously when
// full. Like every ObjectWriter it has a single owner and takes no lock
// (relay-spec §4.8); only DurableOffset may be read from another goroutine.
type objectWriter struct {
	d        *Driver
	ctx      context.Context // from Create, for part uploads inside Write
	key      string
	uploadID string

	buf     []byte // the in-flight part, cap = part size once allocated
	parts   []types.CompletedPart
	durable atomic.Int64 // byte end of the last completed part

	broken     bool // a part upload failed; Resume before writing again
	completing bool // CompleteMultipartUpload was sent; the parts are final
	closed     bool // committed or aborted
	committed  bool
	aborted    bool
}

// Write buffers p into the current part, uploading each part as it fills.
// After an error the writer refuses Write and Commit until Resume.
func (w *objectWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errClosed
	}
	if w.completing {
		return 0, errCompleting
	}
	if w.broken {
		return 0, errNeedsResume
	}
	if w.buf == nil {
		w.buf = make([]byte, 0, w.d.partSize)
	}
	n := 0
	for len(p) > 0 {
		k := copy(w.buf[len(w.buf):cap(w.buf)], p)
		w.buf = w.buf[:len(w.buf)+k]
		n += k
		p = p[k:]
		if len(w.buf) == cap(w.buf) {
			if err := w.uploadPart(w.ctx); err != nil {
				w.broken = true
				return n, err
			}
		}
	}
	return n, nil
}

// uploadPart uploads the buffer as the next part and empties it.
func (w *objectWriter) uploadPart(ctx context.Context) error {
	num := int32(len(w.parts) + 1)
	out, err := w.d.client.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket:        aws.String(w.d.bucket),
		Key:           aws.String(w.d.objectKey(w.key)),
		UploadId:      aws.String(w.uploadID),
		PartNumber:    aws.Int32(num),
		Body:          bytes.NewReader(w.buf),
		ContentLength: aws.Int64(int64(len(w.buf))),
	})
	if err != nil {
		return fmt.Errorf("s3: upload part %d of %q: %w", num, w.key, err)
	}
	w.parts = append(w.parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num)})
	w.durable.Add(int64(len(w.buf)))
	w.buf = w.buf[:0]
	return nil
}

// DurableOffset is the byte end of the last completed part.
func (w *objectWriter) DurableOffset() int64 { return w.durable.Load() }

// Resume discards the in-flight part and returns the end of the last
// completed part, which equals DurableOffset (design §6.2). S3 is stateless
// HTTP, so there is nothing to reconnect; re-uploading a part number replaces
// any copy a lost response left behind.
func (w *objectWriter) Resume(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if w.closed {
		return 0, errClosed
	}
	w.buf = w.buf[:0]
	w.broken = false
	return w.durable.Load(), nil
}

// Commit uploads the final (possibly short) part, then completes the upload.
// A zero-byte object is an error: zrec never produces one.
//
// CompleteMultipartUpload can succeed with its response lost. The retried
// Commit then gets NoSuchUpload; if the object exists with exactly the
// committed size, the earlier attempt published it and Commit succeeds. If
// that HeadObject fails, the error classifies as the HEAD failure does
// (usually Transient), so the caller retries Commit; if the object is missing
// or has the wrong size, the error is Permanent.
//
// Once CompleteMultipartUpload has been sent the part list is final, so Write
// is refused from then on, even after Resume; only Commit and Abort remain.
func (w *objectWriter) Commit(ctx context.Context) error {
	if w.closed {
		return errClosed
	}
	if w.broken {
		return errNeedsResume
	}
	if w.durable.Load()+int64(len(w.buf)) == 0 {
		return fmt.Errorf("s3: commit %q: zero-byte object", w.key)
	}
	if len(w.buf) > 0 {
		if err := w.uploadPart(ctx); err != nil {
			w.broken = true
			return err
		}
	}
	w.completing = true
	_, err := w.d.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(w.d.bucket),
		Key:             aws.String(w.d.objectKey(w.key)),
		UploadId:        aws.String(w.uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: w.parts},
	})
	if err != nil {
		if apiCode(err) != "NoSuchUpload" {
			return fmt.Errorf("s3: commit %q: %w", w.key, err)
		}
		// The class comes from the HeadObject outcome, never from the
		// NoSuchUpload, which is only reported with %v.
		ok, why, serr := w.published(ctx)
		if serr != nil {
			return fmt.Errorf("s3: commit %q: upload is gone (%v) and the object cannot be confirmed: %w", w.key, err, serr)
		}
		if !ok {
			return fmt.Errorf("s3: commit %q: %w: %s (%v)", w.key, errUnpublished, why, err)
		}
	}
	w.closed, w.committed = true, true
	w.buf = nil
	return nil
}

// published reports whether the object exists with exactly the bytes of the
// completed parts. serr is a HeadObject failure other than NotFound, when the
// answer is unknown; otherwise why says what was found when ok is false.
func (w *objectWriter) published(ctx context.Context) (ok bool, why string, serr error) {
	info, err := w.d.stat(ctx, w.key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return false, "the object does not exist", nil
	case err != nil:
		return false, "", err
	case info.Size != w.durable.Load():
		return false, fmt.Sprintf("the object has %d bytes, want %d", info.Size, w.durable.Load()), nil
	}
	return true, "", nil
}

// Abort aborts the multipart upload. It is a no-op once aborted and an error
// once committed.
//
// After a Commit whose CompleteMultipartUpload was sent, NoSuchUpload on Abort
// may mean that request published the object with its response lost. Abort
// then checks with HeadObject: if the object exists with the committed size,
// it reports an error (errAbortCommitted, Permanent) because the object is
// published and Abort cannot undo it (relay credentials cannot delete it,
// D15); if the HEAD fails, Abort returns that error and may be retried.
func (w *objectWriter) Abort(ctx context.Context) error {
	if w.committed {
		return fmt.Errorf("s3: abort %q: already committed", w.key)
	}
	if w.aborted {
		return nil
	}
	w.closed = true
	w.buf = nil
	_, err := w.d.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
		Bucket:   aws.String(w.d.bucket),
		Key:      aws.String(w.d.objectKey(w.key)),
		UploadId: aws.String(w.uploadID),
	})
	if err != nil && apiCode(err) != "NoSuchUpload" {
		return fmt.Errorf("s3: abort %q: %w", w.key, err)
	}
	if err != nil && w.completing {
		ok, _, serr := w.published(ctx)
		if serr != nil {
			return fmt.Errorf("s3: abort %q: upload is gone (%v) and the object cannot be checked: %w", w.key, err, serr)
		}
		if ok {
			w.committed = true
			return fmt.Errorf("s3: abort %q: %w", w.key, errAbortCommitted)
		}
	}
	w.aborted = true
	return nil
}
