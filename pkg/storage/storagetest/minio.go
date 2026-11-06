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
// upstream minio/minio repository is no longer published on Docker Hub, so
// this is the pgsty community build of the same server.
const MinIOImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"

// MinIOBucket is the bucket StartMinIO creates.
const MinIOBucket = "zrelay-test"

// StartMinIO runs a throwaway MinIO container (Docker required) with fresh
// random root credentials and an empty bucket, and terminates it when the
// test ends. endpoint is an http:// URL on the host's mapped port.
func StartMinIO(t *testing.T) (endpoint, accessKey, secretKey, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	accessKey, secretKey = "zrelay-"+randomHex(t, 4), randomHex(t, 20)
	ctr, err := minio.Run(ctx, MinIOImage, minio.WithUsername(accessKey), minio.WithPassword(secretKey))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start MinIO: %v", err)
	}
	hostPort, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("MinIO endpoint: %v", err)
	}
	endpoint = "http://" + hostPort

	client := awss3.New(awss3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	})
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(MinIOBucket)}); err != nil {
		t.Fatalf("create MinIO bucket: %v", err)
	}
	return endpoint, accessKey, secretKey, MinIOBucket
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}
