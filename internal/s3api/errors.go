package s3api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// s3XmlNamespace is the standard S3 XML namespace used in all responses.
const s3XmlNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// S3 error code to HTTP status code mapping.
//
// Any code absent here falls back to 500, which misleads clients into retrying
// what are actually permanent client errors. Keep this in sync with every code
// the handlers and storage engine emit.
var errorHTTPStatus = map[string]int{
	"AccessDenied":                            http.StatusForbidden,
	"AuthorizationHeaderMalformed":            http.StatusBadRequest,
	"BadDigest":                               http.StatusBadRequest,
	"BadRequest":                              http.StatusBadRequest,
	"BucketAlreadyExists":                     http.StatusConflict,
	"BucketAlreadyOwnedByYou":                 http.StatusConflict,
	"BucketNotEmpty":                          http.StatusConflict,
	"EntityTooLarge":                          http.StatusRequestEntityTooLarge,
	"EntityTooSmall":                          http.StatusBadRequest,
	"IncompleteBody":                          http.StatusBadRequest,
	"InternalError":                           http.StatusInternalServerError,
	"IllegalVersioningConfigurationException": http.StatusBadRequest,
	"InvalidAccessKeyId":                      http.StatusForbidden,
	"KeyTooLongError":                         http.StatusBadRequest,
	"InvalidArgument":                         http.StatusBadRequest,
	"InvalidBucketName":                       http.StatusBadRequest,
	"InvalidDigest":                           http.StatusBadRequest,
	"InvalidPart":                             http.StatusBadRequest,
	"InvalidPartOrder":                        http.StatusBadRequest,
	"InvalidRange":                            http.StatusRequestedRangeNotSatisfiable,
	"InvalidRequest":                          http.StatusBadRequest,
	"MalformedXML":                            http.StatusBadRequest,
	"MaxMessageLengthExceeded":                http.StatusBadRequest,
	"MethodNotAllowed":                        http.StatusMethodNotAllowed,
	"NoSuchBucket":                            http.StatusNotFound,
	"NoSuchCORSConfiguration":                 http.StatusNotFound,
	"NoSuchKey":                               http.StatusNotFound,
	"NoSuchLifecycleConfiguration":            http.StatusNotFound,
	"NoSuchUpload":                            http.StatusNotFound,
	"NoSuchVersion":                           http.StatusNotFound,
	"NotImplemented":                          http.StatusNotImplemented,
	"PreconditionFailed":                      http.StatusPreconditionFailed,
	"RequestTimeTooSkewed":                    http.StatusForbidden,
	"SignatureDoesNotMatch":                   http.StatusForbidden,
	"TooManyRequests":                         http.StatusTooManyRequests,
	"XAmzContentSHA256Mismatch":               http.StatusBadRequest,
}

// handleStorageError writes the appropriate S3 error response for a storage error.
// Returns true if the error was handled, false if err is nil.
func handleStorageError(w http.ResponseWriter, err error, resource string) bool {
	if err == nil {
		return false
	}
	var s3Err *storage.S3Error
	var authErr *auth.AuthError
	switch {
	case errors.As(err, &s3Err):
		writeS3Error(w, s3Err.Code, s3Err.Message, resource)
	case errors.As(err, &authErr):
		// Raised while reading a body the SigV4 layer verifies as it streams
		// (a payload hash, chunk signature or checksum failure, or the size
		// cap), which reaches the handler wrapped in a storage write error.
		writeS3Error(w, authErr.Code, authErr.Message, resource)
	default:
		// Never surface internal error text (it leaks filesystem paths) to callers.
		slog.Error("[S3] Internal error", "resource", resource, "error", err)
		writeS3Error(w, "InternalError", "We encountered an internal error. Please try again.", resource)
	}
	return true
}

// writeS3Error writes an S3-compatible XML error response.
func writeS3Error(w http.ResponseWriter, code, message, resource string) {
	status, ok := errorHTTPStatus[code]
	if !ok {
		status = http.StatusInternalServerError
	}

	resp := ErrorResponse{
		Code:      code,
		Message:   message,
		Resource:  resource,
		RequestId: requestIDFor(w),
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(resp)
}

// requestIDHeader carries the per-request ID set by Router.ServeHTTP.
const requestIDHeader = "x-amz-request-id"

// newRequestID returns an ID in the form S3 uses: 16 upper-case hex digits.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// requestIDFor returns the ID of the request being answered, so an error
// body matches its response header and access log entry. A response written
// outside Router.ServeHTTP (as in handler tests) gets a fresh ID.
func requestIDFor(w http.ResponseWriter) string {
	if id := w.Header().Get(requestIDHeader); id != "" {
		return id
	}
	id := newRequestID()
	w.Header().Set(requestIDHeader, id)
	return id
}

// writeXML writes an XML response with the given status code.
func writeXML(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}
