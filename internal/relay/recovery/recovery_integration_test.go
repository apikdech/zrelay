//go:build integration

package recovery_test

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/storage/s3"
	"github.com/apikdech/zrelay-v2/pkg/storage/storagetest"
)

const mib = 1 << 20

var rootSeq atomic.Int64

// TestRecoveryMinIO runs the Contract §7.4 scenarios against a real MinIO,
// where an interrupted stream is a multipart upload.
func TestRecoveryMinIO(t *testing.T) {
	endpoint, accessKey, secretKey, bucket := storagetest.StartMinIO(t)
	runScenarios(t, func(t *testing.T) env {
		t.Helper()
		d, err := s3.New(t.Context(), s3.Config{
			Endpoint:     endpoint,
			Region:       "us-east-1",
			Bucket:       bucket,
			Root:         fmt.Sprintf("cctv-rec-%d", rootSeq.Add(1)),
			AccessKey:    accessKey,
			SecretKey:    secretKey,
			UsePathStyle: true,
			PartSize:     5 * mib,
			HTTPClient:   &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		})
		if err != nil {
			t.Fatalf("s3.New: %v", err)
		}
		return env{
			p: d, r: d,
			// 6 MiB after the header completes one 5 MiB part.
			pad:       6 * mib,
			olderThan: time.Millisecond,
			// An aged staging sidecar is ~3.3 s old when the young-video
			// scenario writes its upload; a 2 s window separates them.
			settle:      2200 * time.Millisecond,
			youngWindow: 2 * time.Second,
			// The s3 writer holds no resources: dropping it leaves the
			// multipart upload and its completed parts on the server.
			crash: func(*testing.T, storage.ObjectWriter) {},
			age: func(t *testing.T, _ ...string) {
				t.Helper()
				// S3 timestamps come from the server and have second
				// resolution; let a second pass so everything written so far
				// is older than olderThan.
				time.Sleep(1100 * time.Millisecond)
			},
		}
	})
}
