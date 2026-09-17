package s3api

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// handleCreateMultipartUpload handles POST /<bucket>/<key>?uploads (CreateMultipartUpload).
func (rt *Router) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	resource := "/" + bucket + "/" + key
	info, err := rt.engine.CreateMultipartUpload(bucket, key, contentType)
	if handleStorageError(w, err, resource) {
		return
	}

	result := InitiateMultipartUploadResult{
		Xmlns:    s3XmlNamespace,
		Bucket:   bucket,
		Key:      key,
		UploadId: info.UploadID,
	}

	writeXML(w, http.StatusOK, result)
}

// handleUploadPart handles PUT /<bucket>/<key>?partNumber=N&uploadId=ID (UploadPart).
func (rt *Router) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	query := r.URL.Query()
	uploadID := query.Get("uploadId")
	partNumberStr := query.Get("partNumber")
	resource := "/" + bucket + "/" + key

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > storage.MaxPartNumber {
		writeS3Error(w, "InvalidArgument",
			fmt.Sprintf("Part number must be an integer between 1 and %d, inclusive.", storage.MaxPartNumber), resource)
		return
	}

	params, err := extractSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}

	ctx := r.Context()
	if params != nil {
		ctx = context.WithValue(ctx, storage.SSECContextKey, params)
	}

	partInfo, err := rt.engine.UploadPart(ctx, bucket, key, uploadID, partNumber, r.Body, r.ContentLength)
	if handleStorageError(w, err, resource) {
		return
	}

	if params != nil {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", params.Algorithm)
		w.Header().Set("x-amz-server-side-encryption-customer-key-MD5", params.KeyMD5)
	}

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, partInfo.ETag))
	w.WriteHeader(http.StatusOK)
}

// handleUploadPartCopy handles PUT /<bucket>/<key>?partNumber=N&uploadId=ID
// with an x-amz-copy-source header (UploadPartCopy). This is distinct from
// handleUploadPart, which reads part data from the request body; here the
// part's content comes from an existing source object instead.
func (rt *Router) handleUploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, key string) {
	query := r.URL.Query()
	uploadID := query.Get("uploadId")
	partNumberStr := query.Get("partNumber")
	resource := "/" + bucket + "/" + key

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > storage.MaxPartNumber {
		writeS3Error(w, "InvalidArgument",
			fmt.Sprintf("Part number must be an integer between 1 and %d, inclusive.", storage.MaxPartNumber), resource)
		return
	}

	src, err := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if err != nil {
		writeS3Error(w, "InvalidArgument", err.Error(), resource)
		return
	}

	srcParams, err := extractCopySourceSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}

	dstParams, err := extractSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}

	srcCtx := r.Context()
	if srcParams != nil {
		srcCtx = context.WithValue(srcCtx, storage.SSECContextKey, srcParams)
	}

	reader, info, err := rt.engine.GetObject(srcCtx, src.Bucket, src.Key, src.VersionID)
	if handleStorageError(w, err, resource) {
		return
	}
	defer reader.Close()

	var body io.Reader = reader
	size := info.Size
	if rangeHeader := r.Header.Get("x-amz-copy-source-range"); rangeHeader != "" {
		ranges, err := parseRange(rangeHeader, info.Size)
		if err != nil || len(ranges) != 1 {
			writeS3Error(w, "InvalidArgument", "The x-amz-copy-source-range value is invalid", resource)
			return
		}
		ra := ranges[0]
		if _, err := io.CopyN(io.Discard, reader, ra.start); err != nil {
			writeS3Error(w, "InternalError", "Failed to read source object", resource)
			return
		}
		body = io.LimitReader(reader, ra.length)
		size = ra.length
	}

	dstCtx := r.Context()
	if dstParams != nil {
		dstCtx = context.WithValue(dstCtx, storage.SSECContextKey, dstParams)
	}

	partInfo, err := rt.engine.UploadPart(dstCtx, bucket, key, uploadID, partNumber, body, size)
	if handleStorageError(w, err, resource) {
		return
	}

	if dstParams != nil {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", dstParams.Algorithm)
		w.Header().Set("x-amz-server-side-encryption-customer-key-MD5", dstParams.KeyMD5)
	}

	result := CopyPartResult{
		LastModified: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		ETag:         fmt.Sprintf(`"%s"`, partInfo.ETag),
	}

	writeXML(w, http.StatusOK, result)
}

// handleCompleteMultipartUpload handles POST /<bucket>/<key>?uploadId=ID (CompleteMultipartUpload).
func (rt *Router) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	resource := "/" + bucket + "/" + key

	var reqBody CompleteMultipartUploadRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxXMLRequestBody)).Decode(&reqBody); err != nil {
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed", resource)
		return
	}

	// SSE-C uploads must re-present the customer key here: Stiva concatenates
	// parts on completion and no longer stores the key server-side.
	params, err := extractSSECParams(r)
	if err != nil {
		writeSSECError(w, err, resource)
		return
	}
	ctx := r.Context()
	if params != nil {
		ctx = context.WithValue(ctx, storage.SSECContextKey, params)
	}

	var parts []storage.CompletePart
	for _, p := range reqBody.Parts {
		etag := p.ETag
		// Strip surrounding quotes if present
		if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
			etag = etag[1 : len(etag)-1]
		}
		parts = append(parts, storage.CompletePart{
			PartNumber: p.PartNumber,
			ETag:       etag,
		})
	}

	info, err := rt.engine.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts)
	if handleStorageError(w, err, resource) {
		return
	}

	result := CompleteMultipartUploadResult{
		Xmlns:    s3XmlNamespace,
		Location: fmt.Sprintf("/%s/%s", bucket, key),
		Bucket:   bucket,
		Key:      key,
		ETag:     fmt.Sprintf(`"%s"`, info.ETag),
	}

	if info.VersionID != "" {
		w.Header().Set("x-amz-version-id", info.VersionID)
	}
	if info.SSECustomerAlgorithm != "" {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", info.SSECustomerAlgorithm)
		w.Header().Set("x-amz-server-side-encryption-customer-key-MD5", info.SSECustomerKeyMD5)
	}

	writeXML(w, http.StatusOK, result)
}

// handleAbortMultipartUpload handles DELETE /<bucket>/<key>?uploadId=ID (AbortMultipartUpload).
func (rt *Router) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	resource := "/" + bucket + "/" + key

	if err := rt.engine.AbortMultipartUpload(bucket, key, uploadID); handleStorageError(w, err, resource) {
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
