package s3

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

var _ storage.Recoverer = (*Driver)(nil)

// ListIncomplete implements storage.Recoverer with ListMultipartUploads,
// keeping uploads under prefix (by whole path element, as List) whose
// Initiated time is at least olderThan ago. Initiated comes from the server's
// clock, so olderThan <= 0 lists every upload regardless of clock skew. Each
// candidate has Key = the final key, UploadID set, Size = the sum of its
// listed parts (one ListParts per upload) and ModTime = Initiated in UTC.
//
// The listing is bucket-wide and filtered here: MinIO's ListMultipartUploads
// matches a non-empty prefix only against the exact object name, so a
// directory prefix would list nothing. Open uploads are few (one per clip in
// flight plus crash leftovers), so this stays cheap.
//
// Only streams are multipart uploads: staging sidecars are PutObject objects
// and are never listed (Contract §7.3 step 0).
func (d *Driver) ListIncomplete(ctx context.Context, prefix string, olderThan time.Duration) ([]storage.Incomplete, error) {
	if prefix != "" {
		if _, err := storage.CleanKey(prefix); err != nil {
			return nil, err
		}
	}
	full := d.objectKey(prefix) // root/prefix, root alone, or ""
	under := func(k string) bool {
		if full == "" {
			return true
		}
		if prefix == "" {
			return strings.HasPrefix(k, full+"/")
		}
		return k == full || strings.HasPrefix(k, full+"/")
	}
	cutoff := time.Now().Add(-olderThan)

	var out []storage.Incomplete
	in := &awss3.ListMultipartUploadsInput{Bucket: aws.String(d.bucket)}
	for {
		page, err := d.client.ListMultipartUploads(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("s3: list incomplete %q: %w", prefix, err)
		}
		for _, u := range page.Uploads {
			k := aws.ToString(u.Key)
			if !under(k) {
				continue
			}
			if d.root != "" {
				k = strings.TrimPrefix(k, d.root+"/")
			}
			initiated := aws.ToTime(u.Initiated).UTC()
			if olderThan > 0 && initiated.After(cutoff) {
				continue
			}
			if _, err := storage.CleanKey(k); err != nil {
				continue // not a key this driver could have written
			}
			id := aws.ToString(u.UploadId)
			_, size, err := d.listParts(ctx, k, id)
			if errors.Is(err, storage.ErrNotFound) {
				continue // completed or aborted since the listing
			}
			if err != nil {
				return nil, err
			}
			out = append(out, storage.Incomplete{Key: k, UploadID: id, Size: size, ModTime: initiated})
		}
		if !aws.ToBool(page.IsTruncated) {
			return out, nil
		}
		next := &awss3.ListMultipartUploadsInput{
			Bucket:         in.Bucket,
			KeyMarker:      page.NextKeyMarker,
			UploadIdMarker: page.NextUploadIdMarker,
		}
		if aws.ToString(next.KeyMarker) == aws.ToString(in.KeyMarker) &&
			aws.ToString(next.UploadIdMarker) == aws.ToString(in.UploadIdMarker) {
			return nil, fmt.Errorf("s3: list incomplete %q: truncated listing did not advance", prefix)
		}
		in = next
	}
}

// listParts returns every uploaded part of the upload, in part-number order,
// and their total size. A missing upload is storage.ErrNotFound.
func (d *Driver) listParts(ctx context.Context, key, uploadID string) ([]types.CompletedPart, int64, error) {
	var (
		parts []types.CompletedPart
		size  int64
	)
	pages := awss3.NewListPartsPaginator(d.client, &awss3.ListPartsInput{
		Bucket:   aws.String(d.bucket),
		Key:      aws.String(d.objectKey(key)),
		UploadId: aws.String(uploadID),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			if apiCode(err) == "NoSuchUpload" {
				return nil, 0, fmt.Errorf("s3: list parts of %q: %w (%v)", key, storage.ErrNotFound, err)
			}
			return nil, 0, fmt.Errorf("s3: list parts of %q: %w", key, err)
		}
		for _, p := range page.Parts {
			parts = append(parts, types.CompletedPart{ETag: p.ETag, PartNumber: p.PartNumber})
			size += aws.ToInt64(p.Size)
		}
	}
	return parts, size, nil
}

// checkCandidate validates an s3 recovery candidate before any request.
func checkCandidate(inc storage.Incomplete) error {
	if _, err := storage.CleanKey(inc.Key); err != nil {
		return err
	}
	if inc.UploadID == "" || inc.StagingKey != "" {
		return fmt.Errorf("%w: %q is not an s3 multipart candidate", storage.ErrInvalidKey, inc.Key)
	}
	return nil
}

// RecoverCommit implements storage.Recoverer: ListParts, then
// CompleteMultipartUpload with the parts that exist. The un-uploaded tail is
// lost (Contract §7.4). With no part, the upload is aborted and
// storage.ErrNothingToRecover returned. The caller reads and validates the
// descriptor from the committed object afterwards (Contract §7.4, s3 order).
//
// A CompleteMultipartUpload whose response was lost leaves the upload gone,
// so the retried RecoverCommit finds NoSuchUpload. If the object then exists
// with exactly the expected size (the parts listed now, or inc.Size from
// ListIncomplete when the upload is already gone), the earlier attempt
// published it and RecoverCommit returns it. Otherwise a gone upload is
// storage.ErrNotFound.
func (d *Driver) RecoverCommit(ctx context.Context, inc storage.Incomplete) (storage.ObjectInfo, error) {
	if err := checkCandidate(inc); err != nil {
		return storage.ObjectInfo{}, err
	}
	parts, size, err := d.listParts(ctx, inc.Key, inc.UploadID)
	if errors.Is(err, storage.ErrNotFound) {
		return d.recoveredEarlier(ctx, inc.Key, inc.Size, err)
	}
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("s3: recover: %w", err)
	}
	if len(parts) == 0 {
		if err := d.AbortIncomplete(ctx, inc); err != nil {
			return storage.ObjectInfo{}, err
		}
		return storage.ObjectInfo{}, storage.ErrNothingToRecover
	}
	_, err = d.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(d.bucket),
		Key:             aws.String(d.objectKey(inc.Key)),
		UploadId:        aws.String(inc.UploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		if apiCode(err) != "NoSuchUpload" {
			return storage.ObjectInfo{}, fmt.Errorf("s3: recover %q: %w", inc.Key, err)
		}
		return d.recoveredEarlier(ctx, inc.Key, size, fmt.Errorf("s3: recover %q: %w (%v)", inc.Key, storage.ErrNotFound, err))
	}
	info, err := d.stat(ctx, inc.Key)
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("s3: recover: %w", err)
	}
	return info, nil
}

// recoveredEarlier decides a recovery whose upload is gone: the object counts
// as recovered only when it exists with exactly want bytes (want > 0).
// Otherwise gone is returned; a HeadObject failure is returned as such, so
// the caller may retry.
func (d *Driver) recoveredEarlier(ctx context.Context, key string, want int64, gone error) (storage.ObjectInfo, error) {
	if want <= 0 {
		return storage.ObjectInfo{}, gone
	}
	info, err := d.stat(ctx, key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return storage.ObjectInfo{}, gone
	case err != nil:
		return storage.ObjectInfo{}, fmt.Errorf("s3: recover: upload is gone (%v) and the object cannot be checked: %w", gone, err)
	case info.Size != want:
		return storage.ObjectInfo{}, fmt.Errorf("%w: the object has %d bytes, want %d", gone, info.Size, want)
	}
	return info, nil
}

// AbortIncomplete aborts the candidate's multipart upload, as recovery does
// for an upload with no staging sidecar (design §6.5). An upload that is
// already gone is not an error.
func (d *Driver) AbortIncomplete(ctx context.Context, inc storage.Incomplete) error {
	if err := checkCandidate(inc); err != nil {
		return err
	}
	_, err := d.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
		Bucket:   aws.String(d.bucket),
		Key:      aws.String(d.objectKey(inc.Key)),
		UploadId: aws.String(inc.UploadID),
	})
	if err != nil && apiCode(err) != "NoSuchUpload" {
		return fmt.Errorf("s3: abort incomplete %q: %w", inc.Key, err)
	}
	return nil
}
