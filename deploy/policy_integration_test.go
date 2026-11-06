//go:build integration

package deploy_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
	"github.com/apikdech/zrelay-v2/pkg/storage/storagetest"
)

// policyFor reads a policy file and points it at the test bucket; the files
// name the example bucket "cctv".
func policyFor(t *testing.T, file, bucket string) []byte {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("cctv")) {
		t.Fatalf("%s does not name the example bucket cctv", file)
	}
	return bytes.ReplaceAll(b, []byte("cctv"), []byte(bucket))
}

func clientFor(endpoint, ak, sk string) *awss3.Client {
	return awss3.New(awss3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(ak, sk, ""),
	})
}

func denied(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "AccessDenied"
}

// TestRelayPolicyDeniesArchiveDelete applies minio-relay-policy.json to a
// MinIO user and checks D15: the relay's credentials delete staging (dot)
// objects only, never archive objects.
func TestRelayPolicyDeniesArchiveDelete(t *testing.T) {
	m := storagetest.StartRestartableMinIO(t)
	m.CreateUser(t, "relay", "relay-secret-1234", "zrelay-relay", policyFor(t, "minio-relay-policy.json", m.Bucket))
	c := clientFor(m.Endpoint, "relay", "relay-secret-1234")
	ctx := context.Background()
	b := aws.String(m.Bucket)

	const archive = "zrelay/clips/c/2026/01/01/x.json"
	const staging = "zrelay/clips/c/2026/01/01/.x.json.part"
	put := func(key string) error {
		_, err := c.PutObject(ctx, &awss3.PutObjectInput{Bucket: b, Key: aws.String(key), Body: strings.NewReader("{}")})
		return err
	}
	del := func(key string) error {
		_, err := c.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: b, Key: aws.String(key)})
		return err
	}

	// What the relay needs.
	if err := put(archive); err != nil {
		t.Fatalf("PutObject on the archive key: %v", err)
	}
	if err := put(staging); err != nil {
		t.Fatalf("PutObject on the staging key: %v", err)
	}
	if _, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: b, Key: aws.String(archive)}); err != nil {
		t.Errorf("GetObject: %v", err)
	}
	if _, err := c.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: b, Prefix: aws.String("zrelay/clips/c/2026/01/01/")}); err != nil {
		t.Errorf("ListObjectsV2 under the root: %v", err)
	}
	up, err := c.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: b, Key: aws.String("zrelay/clips/c/2026/01/01/v.zrec")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if _, err := c.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{Bucket: b, Prefix: aws.String("zrelay/clips/")}); err != nil {
		t.Errorf("ListMultipartUploads under the root: %v", err)
	}
	if _, err := c.ListParts(ctx, &awss3.ListPartsInput{Bucket: b, Key: aws.String("zrelay/clips/c/2026/01/01/v.zrec"), UploadId: up.UploadId}); err != nil {
		t.Errorf("ListParts: %v", err)
	}
	if _, err := c.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{Bucket: b, Key: aws.String("zrelay/clips/c/2026/01/01/v.zrec"), UploadId: up.UploadId}); err != nil {
		t.Errorf("AbortMultipartUpload: %v", err)
	}

	// D15: deleting an archive object is denied, and the object survives.
	if err := del(archive); !denied(err) {
		t.Fatalf("DeleteObject on %s = %v, want AccessDenied", archive, err)
	}
	if _, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: b, Key: aws.String(archive)}); err != nil {
		t.Errorf("archive object gone after a denied delete: %v", err)
	}
	// The staging object may be deleted.
	if err := del(staging); err != nil {
		t.Errorf("DeleteObject on %s: %v", staging, err)
	}

	// Nothing outside the root.
	if err := put("other/x.json"); !denied(err) {
		t.Errorf("PutObject outside the root = %v, want AccessDenied", err)
	}
	if _, err := c.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: b, Prefix: aws.String("other/")}); !denied(err) {
		t.Errorf("ListObjectsV2 outside the root = %v, want AccessDenied", err)
	}
	// HeadBucket (the health probe) is allowed. MinIO treats it, and a listing
	// without a prefix, as "no s3:prefix key", so the policy cannot allow one
	// and deny the other: key names of the whole bucket are listable, which is
	// why the runbook dedicates the bucket to zrelay. Reading and writing
	// stay confined to the root, as checked above.
	if _, err := c.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: b}); err != nil {
		t.Errorf("HeadBucket: %v", err)
	}
}

// TestZviewPolicyIsReadOnlyOnMinIO checks the viewer's credentials against a
// real server: read the archive, write and delete nothing.
func TestZviewPolicyIsReadOnlyOnMinIO(t *testing.T) {
	m := storagetest.StartRestartableMinIO(t)
	m.CreateUser(t, "relay", "relay-secret-1234", "zrelay-relay", policyFor(t, "minio-relay-policy.json", m.Bucket))
	m.CreateUser(t, "zview", "zview-secret-1234", "zrelay-zview", policyFor(t, "minio-zview-policy.json", m.Bucket))
	relay := clientFor(m.Endpoint, "relay", "relay-secret-1234")
	zview := clientFor(m.Endpoint, "zview", "zview-secret-1234")
	ctx := context.Background()
	b := aws.String(m.Bucket)
	const key = "zrelay/clips/c/2026/01/01/x.json"

	if _, err := relay.PutObject(ctx, &awss3.PutObjectInput{Bucket: b, Key: aws.String(key), Body: strings.NewReader("{}")}); err != nil {
		t.Fatal(err)
	}
	if _, err := zview.GetObject(ctx, &awss3.GetObjectInput{Bucket: b, Key: aws.String(key)}); err != nil {
		t.Errorf("zview GetObject: %v", err)
	}
	if _, err := zview.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: b, Prefix: aws.String("zrelay/clips/")}); err != nil {
		t.Errorf("zview ListObjectsV2: %v", err)
	}
	if _, err := zview.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: b}); err != nil {
		t.Errorf("zview HeadBucket: %v", err)
	}
	if _, err := zview.PutObject(ctx, &awss3.PutObjectInput{Bucket: b, Key: aws.String("zrelay/y.json"), Body: strings.NewReader("{}")}); !denied(err) {
		t.Errorf("zview PutObject = %v, want AccessDenied", err)
	}
	if _, err := zview.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: b, Key: aws.String(key)}); !denied(err) {
		t.Errorf("zview DeleteObject = %v, want AccessDenied", err)
	}
}

// TestRelayPolicyWorksWithTheS3Driver runs the relay's real S3 driver, with
// the relay user's credentials, through every call the relay makes: the
// health probe (HeadBucket), the staging sidecar, a multipart upload with its
// commit, the final sidecar, the staging delete, and the recovery listings.
// The policy must allow all of them, and the driver's Delete must still fail
// on an archive key.
func TestRelayPolicyWorksWithTheS3Driver(t *testing.T) {
	m := storagetest.StartRestartableMinIO(t)
	m.CreateUser(t, "relay", "relay-secret-1234", "zrelay-relay", policyFor(t, "minio-relay-policy.json", m.Bucket))
	d, err := s3.New(t.Context(), s3.Config{
		Endpoint: m.Endpoint, Region: "us-east-1", Bucket: m.Bucket, Root: "zrelay",
		AccessKey: "relay", SecretKey: "relay-secret-1234", UsePathStyle: true, PartSize: 5 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := t.Context()

	if err := d.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	const dir = "clips/c/2026/01/01/"
	if err := d.PutSmall(ctx, dir+".x.json.part", []byte("{}"), "application/json"); err != nil {
		t.Fatalf("PutSmall staging: %v", err)
	}
	w, err := d.Create(ctx, dir+"x.zrec")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write(bytes.Repeat([]byte{1}, 6<<20)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// A second, abandoned upload for the recovery listing.
	w2, err := d.Create(ctx, dir+"y.zrec")
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if _, err := w2.Write(bytes.Repeat([]byte{2}, 6<<20)); err != nil {
		t.Fatalf("Write second: %v", err)
	}
	if got, err := d.ListIncomplete(ctx, "clips/c/2026/01/01", 0); err != nil || len(got) != 2 {
		t.Errorf("ListIncomplete = %v, %v; want the 2 open uploads", got, err)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w2.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := d.PutSmall(ctx, dir+"x.json", []byte("{}"), "application/json"); err != nil {
		t.Fatalf("PutSmall final: %v", err)
	}
	if got, err := d.ListStaging(ctx, "clips/c/2026/01/01"); err != nil || len(got) != 1 {
		t.Errorf("ListStaging = %v, %v; want the staging sidecar", got, err)
	}
	if err := d.Delete(ctx, dir+".x.json.part"); err != nil {
		t.Errorf("Delete staging: %v", err)
	}
	if err := d.Delete(ctx, dir+"x.json"); err == nil {
		t.Error("Delete of an archive sidecar succeeded; the relay must not be able to")
	}
	if err := d.Delete(ctx, dir+"x.zrec"); err == nil {
		t.Error("Delete of an archive video succeeded; the relay must not be able to")
	}
}
