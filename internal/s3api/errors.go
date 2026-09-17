package s3api

import (
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
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
	"AccessDenied":                 http.StatusForbidden,
	"AuthorizationHeaderMalformed": http.StatusBadRequest,
	"BadRequest":                   http.StatusBadRequest,
	"BucketAlreadyExists":          http.StatusConflict,
	"BucketAlreadyOwnedByYou":      http.StatusConflict,
	"BucketNotEmpty":               http.StatusConflict,
	"EntityTooLarge":               http.StatusRequestEntityTooLarge,
	"EntityTooSmall":               http.StatusBadRequest,
	"InternalError":                http.StatusInternalServerError,
	"InvalidAccessKeyId":           http.StatusForbidden,
	"InvalidArgument":              http.StatusBadRequest,
	"InvalidBucketName":            http.StatusBadRequest,
	"InvalidDigest":                http.StatusBadRequest,
	"InvalidPart":                  http.StatusBadRequest,
	"InvalidPartOrder":             http.StatusBadRequest,
	"InvalidRange":                 http.StatusRequestedRangeNotSatisfiable,
	"InvalidRequest":               http.StatusBadRequest,
	"MalformedXML":                 http.StatusBadRequest,
	"MethodNotAllowed":             http.StatusMethodNotAllowed,
	"NoSuchBucket":                 http.StatusNotFound,
	"NoSuchCORSConfiguration":      http.StatusNotFound,
	"NoSuchKey":                    http.StatusNotFound,
	"NoSuchLifecycleConfiguration": http.StatusNotFound,
	"NoSuchUpload":                 http.StatusNotFound,
	"NoSuchVersion":                http.StatusNotFound,
	"NotImplemented":               http.StatusNotImplemented,
	"PreconditionFailed":           http.StatusPreconditionFailed,
	"RequestTimeTooSkewed":         http.StatusForbidden,
	"SignatureDoesNotMatch":        http.StatusForbidden,
	"TooManyRequests":              http.StatusTooManyRequests,
}

// handleStorageError writes the appropriate S3 error response for a storage error.
// Returns true if the error was handled, false if err is nil.
func handleStorageError(w http.ResponseWriter, err error, resource string) bool {
	if err == nil {
		return false
	}
	var s3Err *storage.S3Error
	if errors.As(err, &s3Err) {
		writeS3Error(w, s3Err.Code, s3Err.Message, resource)
	} else {
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
		RequestId: uuid.New().String(),
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(resp)
}

// writeXML writes an XML response with the given status code.
func writeXML(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}
