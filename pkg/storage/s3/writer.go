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

	broken    bool // a part upload failed; Resume before writing again
	closed    bool // committed or aborted
	committed bool
	aborted   bool
}

// Write buffers p into the current part, uploading each part as it fills.
// After an error the writer refuses Write and Commit until Resume.
func (w *objectWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errClosed
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
// committed size, the earlier attempt published it and Commit succeeds.
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
	size := w.durable.Load()
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
		info, serr := w.d.stat(ctx, w.key)
		if serr != nil {
			return fmt.Errorf("s3: commit %q: upload is gone (%w) and the object cannot be confirmed: %w", w.key, err, serr)
		}
		if info.Size != size {
			return fmt.Errorf("s3: commit %q: upload is gone (%w) and the object has %d bytes, want %d", w.key, err, info.Size, size)
		}
	}
	w.closed, w.committed = true, true
	w.buf = nil
	return nil
}

// Abort aborts the multipart upload. It is a no-op once aborted and an error
// once committed.
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
	w.aborted = true
	return nil
}
