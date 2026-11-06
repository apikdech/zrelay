// Package s3 is the S3 (MinIO) storage driver (design §6.2, relay-spec §4.9).
//
// Objects live at <bucket>/<root>/<key> (Contract §7.1). Streamed objects are
// multipart uploads, published by CompleteMultipartUpload; small objects are a
// single PutObject, which S3 makes atomic. ETag is the server's entity tag.
//
// The SDK's retryer is disabled (aws.NopRetryer): every call is one attempt,
// and egress owns retries and the circuit breaker (design §6.3), sorting
// errors with Classify.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

const (
	// DefaultPartSize is the multipart part size when Config.PartSize is 0
	// (part_size_mib default 8, design §6.2).
	DefaultPartSize = 8 << 20
	// MinPartSize is the S3 minimum for every part but the last.
	MinPartSize = 5 << 20

	defaultRegion = "us-east-1"
)

// Config configures a Driver. AccessKey and SecretKey are resolved secret
// values, never refs; they are not included in any error.
type Config struct {
	Endpoint, Region, Bucket, Root, AccessKey, SecretKey string

	UsePathStyle bool  // true for MinIO
	PartSize     int64 // bytes; 0 means DefaultPartSize, otherwise >= MinPartSize
	HTTPClient   *http.Client
}

// Driver is the S3 storage.Provider.
type Driver struct {
	client   *awss3.Client
	bucket   string
	root     string
	partSize int64
}

var _ storage.Provider = (*Driver)(nil)

// New validates cfg and returns a driver. It makes no request; HealthCheck
// probes the bucket.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is empty")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: access key and secret key are required")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		switch {
		case err != nil:
			return nil, errors.New("s3: endpoint is not a valid URL")
		case u.User != nil:
			return nil, errors.New("s3: endpoint must not carry userinfo")
		case u.Scheme != "http" && u.Scheme != "https", u.Host == "":
			return nil, errors.New("s3: endpoint must be an http:// or https:// URL")
		}
	}
	if cfg.Root != "" {
		if _, err := storage.CleanKey(cfg.Root); err != nil {
			return nil, fmt.Errorf("s3: root: %w", err)
		}
	}
	partSize := cfg.PartSize
	switch {
	case partSize == 0:
		partSize = DefaultPartSize
	case partSize < MinPartSize:
		return nil, fmt.Errorf("s3: part size %d is below the %d-byte minimum", partSize, MinPartSize)
	}
	region := cfg.Region
	if region == "" {
		region = defaultRegion
	}

	opts := awss3.Options{
		Region:       region,
		UsePathStyle: cfg.UsePathStyle,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		Retryer:      aws.NopRetryer{},
		// Checksums only where S3 requires them: MinIO and R2 differ in
		// their support for the newer flexible checksums.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	if cfg.HTTPClient != nil {
		opts.HTTPClient = cfg.HTTPClient
	}
	return &Driver{
		client:   awss3.New(opts),
		bucket:   cfg.Bucket,
		root:     cfg.Root,
		partSize: partSize,
	}, nil
}

// Name implements storage.ReadProvider.
func (d *Driver) Name() string { return "s3" }

// HealthCheck is HeadBucket. A missing bucket is reported as NoSuchBucket,
// which Classify treats as permanent.
func (d *Driver) HealthCheck(ctx context.Context) error {
	_, err := d.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(d.bucket)})
	if err != nil {
		if Classify(err) == storage.NotFound {
			return fmt.Errorf("s3: health: %w: %w", errNoSuchBucket, err)
		}
		return fmt.Errorf("s3: health: %w", err)
	}
	return nil
}

// Close implements storage.ReadProvider. The driver holds no resources of
// its own; a caller-supplied HTTPClient stays the caller's.
func (d *Driver) Close() error { return nil }

func (d *Driver) objectKey(key string) string { return storage.JoinRoot(d.root, key) }

func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// Stat implements storage.Reader (HeadObject). Dot-names are not filtered.
func (d *Driver) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if _, err := storage.CleanKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	return d.stat(ctx, key)
}

func (d *Driver) stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	out, err := d.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.objectKey(key)),
	})
	if err != nil {
		if Classify(err) == storage.NotFound {
			return storage.ObjectInfo{}, fmt.Errorf("s3: stat %q: %w", key, storage.ErrNotFound)
		}
		return storage.ObjectInfo{}, fmt.Errorf("s3: stat %q: %w", key, err)
	}
	return storage.ObjectInfo{
		Key:     key,
		Size:    aws.ToInt64(out.ContentLength),
		ModTime: aws.ToTime(out.LastModified).UTC(),
		ETag:    aws.ToString(out.ETag),
	}, nil
}

// OpenRange implements storage.Reader with a ranged GetObject. A range
// extending past the end is clamped to the object; an offset beyond the end
// is an error. Total comes from Content-Range.
func (d *Driver) OpenRange(ctx context.Context, key string, off, n int64) (*storage.ObjectRange, error) {
	if _, err := storage.CleanKey(key); err != nil {
		return nil, err
	}
	if off < 0 || n < -1 {
		return nil, fmt.Errorf("s3: open %q: invalid range off=%d n=%d", key, off, n)
	}
	if n == 0 {
		return d.emptyRange(ctx, key, off)
	}
	in := &awss3.GetObjectInput{Bucket: aws.String(d.bucket), Key: aws.String(d.objectKey(key))}
	if off > 0 || n > 0 {
		r := "bytes=" + strconv.FormatInt(off, 10) + "-"
		if n > 0 {
			r += strconv.FormatInt(off+n-1, 10)
		}
		in.Range = aws.String(r)
	}
	out, err := d.client.GetObject(ctx, in)
	if err != nil {
		switch {
		case Classify(err) == storage.NotFound:
			return nil, fmt.Errorf("s3: open %q: %w", key, storage.ErrNotFound)
		case apiCode(err) == "InvalidRange":
			// The offset is at or past the end of the object.
			return d.emptyRange(ctx, key, off)
		}
		return nil, fmt.Errorf("s3: open %q: %w", key, err)
	}
	total := aws.ToInt64(out.ContentLength)
	if cr := aws.ToString(out.ContentRange); cr != "" {
		_, t, _ := strings.Cut(cr, "/")
		v, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			_ = out.Body.Close()
			return nil, fmt.Errorf("s3: open %q: unparseable Content-Range %q", key, cr)
		}
		total = v
	}
	return &storage.ObjectRange{ReadCloser: out.Body, Total: total, ETag: aws.ToString(out.ETag)}, nil
}

// emptyRange answers a range that selects no bytes at off, which is valid
// only when off does not exceed the object's size.
func (d *Driver) emptyRange(ctx context.Context, key string, off int64) (*storage.ObjectRange, error) {
	info, err := d.stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if off > info.Size {
		return nil, fmt.Errorf("s3: open %q: offset %d beyond size %d", key, off, info.Size)
	}
	return &storage.ObjectRange{ReadCloser: io.NopCloser(bytes.NewReader(nil)), Total: info.Size, ETag: info.ETag}, nil
}

// List implements storage.Reader: ListObjectsV2 by byte prefix, filtered to
// whole path elements, with dot-names omitted (relay-spec §4.9).
func (d *Driver) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		if prefix != "" {
			if _, err := storage.CleanKey(prefix); err != nil {
				yield(storage.ObjectInfo{}, err)
				return
			}
		}
		full := d.objectKey(prefix) // root/prefix, or root alone
		byPrefix := full
		if prefix == "" && d.root != "" {
			byPrefix = d.root + "/"
		}
		pages := awss3.NewListObjectsV2Paginator(d.client, &awss3.ListObjectsV2Input{
			Bucket: aws.String(d.bucket),
			Prefix: aws.String(byPrefix),
		})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				yield(storage.ObjectInfo{}, fmt.Errorf("s3: list %q: %w", prefix, err))
				return
			}
			for _, o := range page.Contents {
				k := aws.ToString(o.Key)
				if prefix != "" && k != full && !strings.HasPrefix(k, full+"/") {
					continue
				}
				if d.root != "" {
					k = strings.TrimPrefix(k, d.root+"/")
				}
				if strings.HasPrefix(path.Base(k), ".") {
					continue
				}
				info := storage.ObjectInfo{
					Key:     k,
					Size:    aws.ToInt64(o.Size),
					ModTime: aws.ToTime(o.LastModified).UTC(),
					ETag:    aws.ToString(o.ETag),
				}
				if !yield(info, nil) {
					return
				}
			}
		}
	}
}

// PutSmall implements storage.Writer with one PutObject, which S3 publishes
// atomically and which replaces an existing object.
func (d *Driver) PutSmall(ctx context.Context, key string, data []byte, contentType string) error {
	if _, err := storage.CleanKey(key); err != nil {
		return err
	}
	if len(data) > storage.MaxSmallSize {
		return fmt.Errorf("s3: put %q: %d bytes exceeds the %d-byte limit", key, len(data), storage.MaxSmallSize)
	}
	in := &awss3.PutObjectInput{
		Bucket:        aws.String(d.bucket),
		Key:           aws.String(d.objectKey(key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := d.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3: put %q: %w", key, err)
	}
	return nil
}

// Delete implements storage.Writer (DeleteObject). Deleting a missing object
// is not an error.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if _, err := storage.CleanKey(key); err != nil {
		return err
	}
	_, err := d.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.objectKey(key)),
	})
	if err != nil && Classify(err) != storage.NotFound {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}
