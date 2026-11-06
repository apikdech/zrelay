package storagetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/minio"
)

// MinIOImage is the pinned MinIO server image for integration tests. The
// upstream minio/minio repository is no longer published on Docker Hub (and
// quay.io/minio/minio has no manifest for the last release tag), so this is
// the pgsty community build of the same server. It is a deviation from the
// plan's minio/minio:RELEASE.* pin, pending orchestrator approval. The tag is
// pinned to its multi-arch index digest to limit the third-party supply-chain
// risk.
const MinIOImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"

// MinIOBucket is the bucket StartMinIO creates.
const MinIOBucket = "zrelay-test"

// MinIO is a running MinIO container started by StartRestartableMinIO.
type MinIO struct {
	// Endpoint is an http:// URL on the host's mapped port. Restart may
	// change it, because Docker can map the container port anew.
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string

	ctr *minio.MinioContainer
}

// StartMinIO runs a throwaway MinIO container (Docker required) with fresh
// random root credentials and an empty bucket, and terminates it when the
// test ends. endpoint is an http:// URL on the host's mapped port.
func StartMinIO(t *testing.T) (endpoint, accessKey, secretKey, bucket string) {
	t.Helper()
	m := StartRestartableMinIO(t)
	return m.Endpoint, m.AccessKey, m.SecretKey, m.Bucket
}

// StartRestartableMinIO is StartMinIO for tests that restart the server: the
// returned MinIO offers Restart.
func StartRestartableMinIO(t *testing.T) *MinIO {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	m := &MinIO{AccessKey: "zrelay-" + randomHex(t, 4), SecretKey: randomHex(t, 20), Bucket: MinIOBucket}
	ctr, err := minio.Run(ctx, MinIOImage, minio.WithUsername(m.AccessKey), minio.WithPassword(m.SecretKey))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start MinIO: %v", err)
	}
	m.ctr = ctr
	m.Endpoint = m.endpoint(ctx, t)

	if _, err := m.client().CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(m.Bucket)}); err != nil {
		t.Fatalf("create MinIO bucket: %v", err)
	}
	return m
}

func (m *MinIO) endpoint(ctx context.Context, t *testing.T) string {
	t.Helper()
	hostPort, err := m.ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("MinIO endpoint: %v", err)
	}
	return "http://" + hostPort
}

func (m *MinIO) client() *awss3.Client {
	return awss3.New(awss3.Options{
		BaseEndpoint: aws.String(m.Endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(m.AccessKey, m.SecretKey, ""),
	})
}

// Restart stops and starts the container, as a power cut and reboot of the
// NAS would: data on disk survives, the server process's memory (including
// its cache of in-progress multipart uploads) does not. It returns once the
// bucket answers again. Endpoint is updated, so callers must rebuild their
// clients from it.
func (m *MinIO) Restart(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	grace := 10 * time.Second
	if err := m.ctr.Stop(ctx, &grace); err != nil {
		t.Fatalf("stop MinIO: %v", err)
	}
	if err := m.ctr.Start(ctx); err != nil {
		t.Fatalf("start MinIO again: %v", err)
	}
	m.Endpoint = m.endpoint(ctx, t)
	c := m.client()
	for {
		_, err := c.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(m.Bucket)})
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("MinIO bucket not ready after restart: %v", err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}
