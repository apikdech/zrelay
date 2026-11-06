package s3

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

var _ storage.Recoverer = (*Driver)(nil)

// stagingSuffix and finalSuffix map a staging sidecar to its video:
// ".<base>.json.part" belongs to "<base>.zrec" (Contract §7.2, §7.3).
const (
	stagingSuffix = ".json.part"
	finalSuffix   = ".zrec"
)

// ListIncomplete implements storage.Recoverer: it lists the multipart uploads
// under prefix (by whole path element, as List) whose Initiated time is at
// least olderThan ago. Initiated comes from the server's clock, so
// olderThan <= 0 lists every upload regardless of clock skew. Each candidate
// has Key = the final key, UploadID set, Size = the sum of its listed parts
// (one ListParts per upload) and ModTime = Initiated in UTC.
//
// A bucket-wide ListMultipartUploads is NOT reliable on MinIO, the
// production target (design D11): with an empty prefix MinIO answers from an
// in-memory cache of the uploads started since the server process began, so
// after a restart (the normal power-cut case) it lists none of the uploads
// that survived on disk. A non-empty prefix is matched against the exact
// object name only, and that lookup does survive restarts. So the candidates
// come from two sources, united and deduplicated by upload id:
//
//   - for every staging sidecar ".<base>.json.part" under prefix
//     (Contract §7.3 step 0, listed with ListObjectsV2 including dot-names),
//     the uploads of the exact key "<base>.zrec" next to it;
//   - the bucket-wide listing with no prefix, which still finds uploads that
//     have no sidecar for as long as the server's cache has them.
//
// An upload that has neither a sidecar nor a cache entry cannot be found
// after a MinIO restart. Recovery would abort it anyway (design §6.5, upload
// with no staging sidecar), and the server's stale-upload expiry reaps it.
//
// Only streams are multipart uploads: staging sidecars are PutObject objects
// and are never listed as uploads (Contract §7.3 step 0).
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

	var (
		out  []storage.Incomplete
		seen = map[string]bool{}
	)
	consider := func(u types.MultipartUpload) error {
		k := aws.ToString(u.Key)
		id := aws.ToString(u.UploadId)
		if !under(k) || seen[id] {
			return nil
		}
		seen[id] = true
		if d.root != "" {
			k = strings.TrimPrefix(k, d.root+"/")
		}
		initiated := aws.ToTime(u.Initiated).UTC()
		if olderThan > 0 && initiated.After(cutoff) {
			return nil
		}
		if _, err := storage.CleanKey(k); err != nil {
			return nil // not a key this driver could have written
		}
		_, size, err := d.listParts(ctx, k, id)
		if errors.Is(err, storage.ErrNotFound) {
			return nil // completed or aborted since the listing
		}
		if err != nil {
			return err
		}
		out = append(out, storage.Incomplete{Key: k, UploadID: id, Size: size, ModTime: initiated})
		return nil
	}

	staged, err := d.ListStaging(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, s := range staged {
		dir, name := path.Split(s.Key)
		base := strings.TrimSuffix(name, stagingSuffix)[1:] // ".<base>.json.part"
		if err := d.eachUpload(ctx, prefix, d.objectKey(dir+base+finalSuffix), consider); err != nil {
			return nil, err
		}
	}
	if err := d.eachUpload(ctx, prefix, "", consider); err != nil {
		return nil, err
	}
	return out, nil
}

// ListStaging implements storage.Recoverer: the staging sidecars
// ".<name>.json.part" (Contract §7.3 step 0) under prefix, by whole path
// element. The public List omits dot-names, so this lists the bucket directly
// with ListObjectsV2 and keeps the objects whose base name begins with "."
// and ends in ".json.part". Keys are relative to the root.
func (d *Driver) ListStaging(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	if prefix != "" {
		if _, err := storage.CleanKey(prefix); err != nil {
			return nil, err
		}
	}
	full := d.objectKey(prefix) // root/prefix, root alone, or ""
	byPrefix := full
	if prefix == "" && d.root != "" {
		byPrefix = d.root + "/"
	}
	var out []storage.ObjectInfo
	pages := awss3.NewListObjectsV2Paginator(d.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket),
		Prefix: aws.String(byPrefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: list staging sidecars under %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			k := aws.ToString(o.Key)
			if prefix != "" && k != full && !strings.HasPrefix(k, full+"/") {
				continue
			}
			if d.root != "" {
				if !strings.HasPrefix(k, d.root+"/") {
					continue
				}
				k = strings.TrimPrefix(k, d.root+"/")
			}
			base, ok := strings.CutSuffix(path.Base(k), stagingSuffix)
			if !ok || !strings.HasPrefix(base, ".") || len(base) < 2 {
				continue
			}
			out = append(out, storage.ObjectInfo{
				Key:     k,
				Size:    aws.ToInt64(o.Size),
				ModTime: aws.ToTime(o.LastModified).UTC(),
				ETag:    aws.ToString(o.ETag),
			})
		}
	}
	return out, nil
}

// eachUpload pages through ListMultipartUploads with the given server-side
// prefix (empty: bucket-wide) and calls fn for every upload. what names the
// caller's prefix in errors.
func (d *Driver) eachUpload(ctx context.Context, what, serverPrefix string, fn func(types.MultipartUpload) error) error {
	in := &awss3.ListMultipartUploadsInput{Bucket: aws.String(d.bucket)}
	if serverPrefix != "" {
		in.Prefix = aws.String(serverPrefix)
	}
	for {
		page, err := d.client.ListMultipartUploads(ctx, in)
		if err != nil {
			return fmt.Errorf("s3: list incomplete %q: %w", what, err)
		}
		for _, u := range page.Uploads {
			if err := fn(u); err != nil {
				return err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return nil
		}
		next := &awss3.ListMultipartUploadsInput{
			Bucket:         in.Bucket,
			Prefix:         in.Prefix,
			KeyMarker:      page.NextKeyMarker,
			UploadIdMarker: page.NextUploadIdMarker,
		}
		if aws.ToString(next.KeyMarker) == aws.ToString(in.KeyMarker) &&
			aws.ToString(next.UploadIdMarker) == aws.ToString(in.UploadIdMarker) {
			return fmt.Errorf("s3: list incomplete %q: truncated listing did not advance", what)
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
