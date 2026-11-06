package s3

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// errNoSuchBucket is what HealthCheck reports for a HeadBucket 404, whose
// response has no body to carry the NoSuchBucket code.
var errNoSuchBucket = errors.New("s3: NoSuchBucket")

// Classify sorts a driver error into the retry classes of design §6.3, for
// egress and its circuit breaker:
//
//   - Transient: network and transport errors, deadlines and cancellation,
//     5xx, 429, RequestTimeout and SlowDown.
//   - Permanent: 403, NoSuchBucket, InvalidAccessKeyId, SignatureDoesNotMatch,
//     any other 4xx, invalid keys, driver misuse (a closed writer, Write after
//     CompleteMultipartUpload was sent), an upload that is gone without its
//     object, and an Abort that found the object already published.
//   - NotFound: NoSuchKey, any other 404 (including NoSuchUpload) and
//     storage.ErrNotFound.
func Classify(err error) storage.ErrorClass {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return storage.NotFound
	case errors.Is(err, storage.ErrInvalidKey), errors.Is(err, errNoSuchBucket),
		errors.Is(err, errUnpublished), errors.Is(err, errAbortCommitted),
		errors.Is(err, errCompleting):
		return storage.Permanent
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "RequestTimeout", "SlowDown", "InternalError", "ServiceUnavailable":
			return storage.Transient
		case "NoSuchBucket", "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied":
			return storage.Permanent
		case "NoSuchKey":
			return storage.NotFound
		}
	}

	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		switch code := status.HTTPStatusCode(); {
		case code >= 500, code == http.StatusTooManyRequests, code == http.StatusRequestTimeout:
			return storage.Transient
		case code == http.StatusNotFound:
			return storage.NotFound
		case code >= 400:
			return storage.Permanent
		}
	}

	var (
		sendErr  *smithyhttp.RequestSendError
		netErr   net.Error
		deserErr *smithy.DeserializationError
	)
	switch {
	case errors.As(err, &sendErr), errors.As(err, &netErr),
		errors.As(err, &deserErr),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return storage.Transient
	}
	return storage.Permanent
}
