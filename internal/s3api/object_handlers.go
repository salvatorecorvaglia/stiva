package s3api

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

const (
	amzSSECAlgorithmHeader = "x-amz-server-side-encryption-customer-algorithm"
	amzSSECKeyMD5Header    = "x-amz-server-side-encryption-customer-key-MD5"
	contentTypeHeader      = "Content-Type"
	amzVersionIDHeader     = "x-amz-version-id"
	amzDeleteMarkerHeader  = "x-amz-delete-marker"
)

// extractSSECParams extracts SSE-C headers from the HTTP request.
func extractSSECParams(r *http.Request) (*storage.SSECParams, error) {
	algo := r.Header.Get(amzSSECAlgorithmHeader)
	if algo == "" {
		return nil, nil
	}
	if algo != "AES256" {
		return nil, fmt.Errorf("UnsupportedEncryptionAlgorithm: The encryption algorithm specified is not supported")
	}
	return buildSSECParams(algo,
		r.Header.Get("x-amz-server-side-encryption-customer-key"),
		r.Header.Get(amzSSECKeyMD5Header))
}

// buildSSECParams validates a base64 customer key against its stated MD5.
// Shared by the object and copy-source header families.
func buildSSECParams(algo, keyB64, keyMD5B64 string) (*storage.SSECParams, error) {
	if keyB64 == "" {
		return nil, fmt.Errorf("MissingEncryptionKey: The encryption key is missing")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("InvalidEncryptionKey: The encryption key is invalid")
	}
	if keyMD5B64 == "" {
		return nil, fmt.Errorf("MissingEncryptionKeyMD5: The encryption key MD5 is missing")
	}
	expectedMD5Raw := md5.Sum(key)
	expectedMD5 := base64.StdEncoding.EncodeToString(expectedMD5Raw[:])
	// Constant-time: this compares a caller-supplied value against a digest
	// derived from the caller's own key, but keep the primitive consistent.
	if subtle.ConstantTimeCompare([]byte(keyMD5B64), []byte(expectedMD5)) != 1 {
		return nil, fmt.Errorf("InvalidEncryptionKeyMD5: The customer-provided encryption key MD5 does not match")
	}
	return &storage.SSECParams{
		Algorithm: algo,
		Key:       key,
		KeyMD5:    keyMD5B64,
	}, nil
}

// sseCErrorCodes lists the "<Code>: <message>" prefixes extractSSECParams/
// buildSSECParams/extractCopySourceSSECParams use, matching the specific S3
// error codes real S3 returns for each condition (some client tooling
// branches on these) rather than the generic InvalidArgument every SSE-C
// validation failure was previously collapsed into.
var sseCErrorCodes = map[string]bool{
	"UnsupportedEncryptionAlgorithm": true,
	"MissingEncryptionKey":           true,
	"InvalidEncryptionKey":           true,
	"MissingEncryptionKeyMD5":        true,
	"InvalidEncryptionKeyMD5":        true,
}

// writeSSECError writes an S3 error response for an SSE-C validation
// failure, using the specific error code encoded in the error message
// (see sseCErrorCodes) instead of a generic InvalidArgument.
func writeSSECError(w http.ResponseWriter, err error, resource string) {
	msg := err.Error()
	code := "InvalidArgument"
	if prefix, rest, ok := strings.Cut(msg, ": "); ok && sseCErrorCodes[prefix] {
		code = prefix
		msg = rest
	}
	writeS3Error(w, code, msg, resource)
}

// handlePutObject handles PUT /<bucket>/<key> (PutObject).
func (rt *Router) handlePutObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	contentType := r.Header.Get(contentTypeHeader)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	size := r.ContentLength
	resource := "/" + bucket + "/" + key

	params, err := extractSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}

	ctx := r.Context()
	if params != nil {
		ctx = context.WithValue(ctx, storage.SSECContextKey, params)
	}

	info, err := rt.engine.PutObject(ctx, bucket, key, r.Body, size, contentType)
	if handleStorageError(w, err, resource) {
		return
	}

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, info.ETag))
	if info.VersionID != "" {
		w.Header().Set(amzVersionIDHeader, info.VersionID)
	}
	if info.SSECustomerAlgorithm != "" {
		w.Header().Set(amzSSECAlgorithmHeader, info.SSECustomerAlgorithm)
		w.Header().Set(amzSSECKeyMD5Header, info.SSECustomerKeyMD5)
	}
	w.WriteHeader(http.StatusOK)
}

// handleGetObject handles GET /<bucket>/<key> (GetObject).
func (rt *Router) handleGetObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	resource := "/" + bucket + "/" + key
	versionID := r.URL.Query().Get("versionId")

	params, err := extractSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}

	ctx := r.Context()
	if params != nil {
		ctx = context.WithValue(ctx, storage.SSECContextKey, params)
	}

	reader, info, err := rt.engine.GetObject(ctx, bucket, key, versionID)
	if err != nil {
		if info != nil && info.IsDeleteMarker {
			w.Header().Set(amzDeleteMarkerHeader, "true")
			w.Header().Set(amzVersionIDHeader, info.VersionID)
			writeS3Error(w, "NoSuchKey", "The specified key does not exist.", resource)
			return
		}
		if handleStorageError(w, err, resource) {
			return
		}
		return
	}
	defer reader.Close()

	w.Header().Set(contentTypeHeader, info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, info.ETag))
	w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	if info.VersionID != "" {
		w.Header().Set(amzVersionIDHeader, info.VersionID)
	}
	if info.SSECustomerAlgorithm != "" {
		w.Header().Set(amzSSECAlgorithmHeader, info.SSECustomerAlgorithm)
		w.Header().Set(amzSSECKeyMD5Header, info.SSECustomerKeyMD5)
	}
	// Content is caller-supplied, so never let a browser sniff it into
	// something executable.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// The response-header overrides let a caller restate an object's
	// Content-Type and Content-Disposition. On an unauthenticated public-bucket
	// read that turns any stored object into arbitrary HTML served from this
	// origin, so they are honoured only for signed requests — which is also
	// where S3 supports them.
	if !isPublicRead(r) {
		applyResponseHeaderOverrides(w, r.URL.Query())
	}

	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		quotedETag := fmt.Sprintf(`"%s"`, info.ETag)
		if ifMatch != "*" && ifMatch != info.ETag && ifMatch != quotedETag {
			writeS3Error(w, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold", resource)
			return
		}
	}
	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		quotedETag := fmt.Sprintf(`"%s"`, info.ETag)
		if ifNoneMatch == "*" || ifNoneMatch == info.ETag || ifNoneMatch == quotedETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// If the reader supports seeking, use http.ServeContent which handles
	// Range requests, conditional headers, and status codes automatically.
	if rs, ok := reader.(io.ReadSeeker); ok {
		http.ServeContent(w, r, key, info.LastModified, rs)
		return
	}

	// Fallback for non-seekable readers: check if client requested Range
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		ranges, err := parseRange(rangeHeader, info.Size)
		if err != nil {
			writeS3Error(w, "InvalidRange", "The requested range is not satisfiable", resource)
			return
		}
		if len(ranges) == 1 {
			ra := ranges[0]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", ra.start, ra.start+ra.length-1, info.Size))
			w.Header().Set("Content-Length", strconv.FormatInt(ra.length, 10))
			w.WriteHeader(http.StatusPartialContent)

			// Discard the initial bytes
			_, _ = io.CopyN(io.Discard, reader, ra.start)
			// Copy only the requested length
			_, _ = io.CopyN(w, reader, ra.length)
			return
		}
	}

	// Fallback for non-seekable readers: serve full content only.
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

// applyResponseHeaderOverrides implements the S3 "response header override"
// query parameters (commonly attached to presigned GetObject URLs). Without
// this, a presigned link always renders inline per the object's stored
// Content-Type — including in a browser tab, where there is otherwise no way
// to force a download for a URL that isn't same-origin with the requester.
//
// Callers must gate this on the request being signed: see handleGetObject.
func applyResponseHeaderOverrides(w http.ResponseWriter, query url.Values) {
	overrides := map[string]string{
		"response-content-disposition": "Content-Disposition",
		"response-content-type":        contentTypeHeader,
		"response-content-language":    "Content-Language",
		"response-content-encoding":    "Content-Encoding",
		"response-cache-control":       "Cache-Control",
		"response-expires":             "Expires",
	}
	for param, header := range overrides {
		if v := query.Get(param); v != "" {
			w.Header().Set(header, v)
		}
	}
}

type httpRange struct {
	start, length int64
}

func parseRange(s string, size int64) ([]httpRange, error) {
	if s == "" {
		return nil, nil
	}
	const b = "bytes="
	if !strings.HasPrefix(s, b) {
		return nil, fmt.Errorf("invalid range")
	}
	s = s[len(b):]
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid range")
	}
	var start, end int64
	var err error
	if parts[0] == "" {
		// -suffix
		end = size - 1
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return nil, fmt.Errorf("invalid range")
		}
		start = size - suffix
		if start < 0 {
			start = 0
		}
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 {
			return nil, fmt.Errorf("invalid range")
		}
		if parts[1] == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || end < start {
				return nil, fmt.Errorf("invalid range")
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	if start >= size {
		return nil, fmt.Errorf("range out of bounds")
	}
	return []httpRange{{start: start, length: end - start + 1}}, nil
}

// handleHeadObject handles HEAD /<bucket>/<key> (HeadObject).
func (rt *Router) handleHeadObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	resource := "/" + bucket + "/" + key
	versionID := r.URL.Query().Get("versionId")

	params, err := extractSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}

	ctx := r.Context()
	if params != nil {
		ctx = context.WithValue(ctx, storage.SSECContextKey, params)
	}

	info, err := rt.engine.HeadObject(ctx, bucket, key, versionID)
	if err != nil {
		if info != nil && info.IsDeleteMarker {
			w.Header().Set(amzDeleteMarkerHeader, "true")
			w.Header().Set(amzVersionIDHeader, info.VersionID)
			writeS3Error(w, "NoSuchKey", "The specified key does not exist.", resource)
			return
		}
		if handleStorageError(w, err, resource) {
			return
		}
	}

	w.Header().Set(contentTypeHeader, info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, info.ETag))
	w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	if info.VersionID != "" {
		w.Header().Set(amzVersionIDHeader, info.VersionID)
	}
	if info.SSECustomerAlgorithm != "" {
		w.Header().Set(amzSSECAlgorithmHeader, info.SSECustomerAlgorithm)
		w.Header().Set(amzSSECKeyMD5Header, info.SSECustomerKeyMD5)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		quotedETag := fmt.Sprintf(`"%s"`, info.ETag)
		if ifMatch != "*" && ifMatch != info.ETag && ifMatch != quotedETag {
			writeS3Error(w, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold", resource)
			return
		}
	}
	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		quotedETag := fmt.Sprintf(`"%s"`, info.ETag)
		if ifNoneMatch == "*" || ifNoneMatch == info.ETag || ifNoneMatch == quotedETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (rt *Router) handleDeleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	versionID := r.URL.Query().Get("versionId")
	isDeleteMarker, delVersionID, err := rt.engine.DeleteObject(bucket, key, versionID)
	resource := "/" + bucket + "/" + key

	if err != nil {
		if handleStorageError(w, err, resource) {
			return
		}
	}

	// If a delete marker was created, S3 returns 204 with delete marker headers
	if err == nil {
		if versionID == "" && isDeleteMarker {
			w.Header().Set(amzDeleteMarkerHeader, "true")
			if delVersionID != "" {
				w.Header().Set(amzVersionIDHeader, delVersionID)
			}
		} else if versionID != "" {
			w.Header().Set(amzVersionIDHeader, versionID)
		}
	}

	// S3 returns 204 even if the key doesn't exist
	w.WriteHeader(http.StatusNoContent)
}

// handleCopyObject handles PUT /<bucket>/<key> with x-amz-copy-source header (CopyObject).
func (rt *Router) handleCopyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	resource := "/" + bucket + "/" + key

	src, err := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if err != nil {
		writeS3Error(w, "InvalidArgument", err.Error(), resource)
		return
	}

	// The source may be SSE-C encrypted under a different key than the
	// destination; S3 carries it in the x-amz-copy-source-* header family.
	srcParams, err := extractCopySourceSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}
	src.SSEC = srcParams

	dstParams, err := extractSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}
	ctx := r.Context()
	if dstParams != nil {
		ctx = context.WithValue(ctx, storage.SSECContextKey, dstParams)
	}

	info, err := rt.engine.CopyObject(ctx, src, bucket, key)
	if handleStorageError(w, err, resource) {
		return
	}

	result := CopyObjectResult{
		LastModified: info.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
		ETag:         fmt.Sprintf(`"%s"`, info.ETag),
	}

	writeXML(w, http.StatusOK, result)
}

// parseCopySource interprets an x-amz-copy-source header.
//
// The header is URL-encoded by AWS SDKs, and may carry ?versionId=. Neither was
// handled before: the value was used verbatim, so any source key containing a
// space, '+' or percent escape resolved to the wrong object (or none at all).
func parseCopySource(raw string) (storage.CopySource, error) {
	if raw == "" {
		return storage.CopySource{}, fmt.Errorf("x-amz-copy-source is required")
	}

	value := strings.TrimPrefix(raw, "/")

	// Split the version selector off before decoding so that a literal
	// "?versionId=" inside a key cannot be confused for the real one.
	versionID := ""
	if idx := strings.LastIndex(value, "?versionId="); idx != -1 {
		versionID = value[idx+len("?versionId="):]
		value = value[:idx]
	}

	bucket, encodedKey, found := strings.Cut(value, "/")
	if !found || bucket == "" || encodedKey == "" {
		return storage.CopySource{}, fmt.Errorf("invalid copy source")
	}

	key, err := url.PathUnescape(encodedKey)
	if err != nil {
		return storage.CopySource{}, fmt.Errorf("invalid copy source encoding")
	}

	return storage.CopySource{Bucket: bucket, Key: key, VersionID: versionID}, nil
}

// extractCopySourceSSECParams reads the SSE-C headers describing the *source*
// object of a copy.
func extractCopySourceSSECParams(r *http.Request) (*storage.SSECParams, error) {
	algo := r.Header.Get("x-amz-copy-source-server-side-encryption-customer-algorithm")
	if algo == "" {
		return nil, nil
	}
	if algo != "AES256" {
		return nil, fmt.Errorf("UnsupportedEncryptionAlgorithm: The encryption algorithm specified is not supported")
	}

	keyB64 := r.Header.Get("x-amz-copy-source-server-side-encryption-customer-key")
	keyMD5 := r.Header.Get("x-amz-copy-source-server-side-encryption-customer-key-MD5")
	return buildSSECParams(algo, keyB64, keyMD5)
}
