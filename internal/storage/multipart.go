package storage

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (fs *FilesystemEngine) CreateMultipartUpload(bucket, key, contentType string) (*MultipartUploadInfo, error) {
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, err
	}

	if _, err := fs.objectPath(bucket, key); err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}

	exists, err := fs.metadata.BucketExists(bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &S3Error{Code: "NoSuchBucket", Message: errBucketNotFound}
	}

	uploadID := uuid.New().String()
	partDir, err := fs.multipartDir(bucket, key, uploadID)
	if err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	baseDir := filepath.Clean(filepath.Join(fs.dataDir, "multipart", bucket))
	basePrefix := baseDir + string(filepath.Separator)
	if !strings.HasPrefix(partDir, basePrefix) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidUploadID}
	}
	if err := os.MkdirAll(partDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create multipart directory: %w", err)
	}

	meta := &MultipartMeta{
		UploadID:    uploadID,
		Bucket:      bucket,
		Key:         key,
		ContentType: contentType,
		Created:     time.Now().UTC(),
	}

	if err := fs.metadata.PutMultipartMeta(meta); err != nil {
		return nil, err
	}

	GlobalMetrics.IncActiveMultiparts()

	return &MultipartUploadInfo{
		UploadID: uploadID,
		Bucket:   bucket,
		Key:      key,
		Created:  meta.Created,
	}, nil
}

// MaxPartNumber is the highest part number S3 accepts for a multipart upload.
//
// Enforcing it here rather than only in the handlers matters: the parts slice
// lives in the upload's metadata record, and PutMultipartMeta rewrites that
// whole JSON blob on every part. An unbounded part-number space therefore
// meant unbounded metadata growth and O(n^2) metadata writes.
const MaxPartNumber = 10000

func (fs *FilesystemEngine) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (*PartInfo, error) {
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, err
	}
	if partNumber < 1 || partNumber > MaxPartNumber {
		return nil, &S3Error{
			Code:    "InvalidArgument",
			Message: fmt.Sprintf("Part number must be an integer between 1 and %d, inclusive.", MaxPartNumber),
		}
	}

	if _, err := fs.objectPath(bucket, key); err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	// See PutObject: this applies regardless of signing mode, including
	// UNSIGNED-PAYLOAD, which bypasses the SigV4 layer's own size check.
	if fs.maxObjectSize > 0 && size > fs.maxObjectSize {
		return nil, &S3Error{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed part size."}
	}

	// 1. Lock briefly to get/initialize multipart metadata and validate SSE-C params
	var ssecParams *SSECParams
	if ctx != nil {
		if params, ok := ctx.Value(SSECContextKey).(*SSECParams); ok && params != nil {
			ssecParams = params
		}
	}

	// Brief lock to read and initialize metadata parameters
	var meta *MultipartMeta
	err := func() error {
		unlock := fs.lockUpload(uploadID)
		defer unlock()

		var err error
		meta, err = fs.metadata.GetMultipartMeta(bucket, key, uploadID)
		if err != nil {
			return err
		}

		// First part upload with SSE-C: record the algorithm and key MD5 so
		// later parts can be checked against it. The key itself is never stored.
		if meta.SSECustomerAlgorithm == "" && len(meta.Parts) == 0 && ssecParams != nil {
			meta.SSECustomerAlgorithm = ssecParams.Algorithm
			meta.SSECustomerKeyMD5 = ssecParams.KeyMD5
			if err := fs.metadata.PutMultipartMeta(meta); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return nil, &S3Error{Code: "NoSuchUpload", Message: errUploadNotFound}
	}

	// SSE-C validation
	if meta.SSECustomerAlgorithm != "" && ssecParams == nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: "The object was stored using a Server-side Encryption with Customer-provided Keys (SSE-C) and cannot be retrieved without it"}
	}
	if meta.SSECustomerAlgorithm == "" && len(meta.Parts) > 0 && ssecParams != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: "The object was not stored using a Server-side Encryption with Customer-provided Keys (SSE-C) and cannot be retrieved with it"}
	}
	if meta.SSECustomerAlgorithm != "" && ssecParams != nil {
		if subtle.ConstantTimeCompare([]byte(ssecParams.KeyMD5), []byte(meta.SSECustomerKeyMD5)) != 1 {
			return nil, &S3Error{Code: "InvalidDigest", Message: "The customer-provided encryption key MD5 does not match"}
		}
	}

	// Serializes the write-rename-metadata sequence below for this exact part
	// number, so a retried/duplicate UploadPart call for the same part can't
	// leave the on-disk file from one request paired with the ETag/size
	// metadata from another. Other part numbers of the same upload are
	// unaffected and continue uploading in parallel.
	unlockPart := fs.lockPart(uploadID, partNumber)
	defer unlockPart()

	partDir, err := fs.multipartDir(bucket, key, uploadID)
	if err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	baseDir := filepath.Clean(filepath.Join(fs.dataDir, "multipart", bucket))
	basePrefix := baseDir + string(filepath.Separator)
	if !strings.HasPrefix(partDir, basePrefix) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidUploadID}
	}
	partPath := filepath.Join(partDir, fmt.Sprintf("part-%05d", partNumber))

	tmpFile, err := os.CreateTemp(partDir, ".part-tmp-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp part file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()

	var iv []byte
	if ssecParams != nil {
		iv = make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return nil, fmt.Errorf(errFailedGenerateIV, err)
		}
		if _, err := tmpFile.Write(iv); err != nil {
			return nil, fmt.Errorf("failed to write IV: %w", err)
		}
	}

	bufWriter := bufio.NewWriterSize(tmpFile, 64*1024)
	var out io.Writer = bufWriter

	if ssecParams != nil {
		block, err := aes.NewCipher(ssecParams.Key)
		if err != nil {
			return nil, fmt.Errorf(errFailedCreateAES, err)
		}
		stream := cipher.NewCTR(block, iv)
		out = &cipher.StreamWriter{S: stream, W: out}
	}

	hash := md5.New()
	capped := reader
	if fs.maxObjectSize > 0 {
		capped = io.LimitReader(reader, fs.maxObjectSize+1)
	}
	written, err := io.Copy(io.MultiWriter(out, hash), capped)
	if err != nil {
		return nil, fmt.Errorf("failed to write part data: %w", err)
	}
	if fs.maxObjectSize > 0 && written > fs.maxObjectSize {
		return nil, &S3Error{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed part size."}
	}

	if err := bufWriter.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush part buffer: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp part file: %w", err)
	}

	if size >= 0 && written != size {
		return nil, &S3Error{Code: "BadRequest", Message: fmt.Sprintf("Size mismatch: expected %d bytes, wrote %d bytes", size, written)}
	}

	if !strings.HasPrefix(partPath, basePrefix) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidUploadID}
	}
	if err := os.Rename(tmpPath, partPath); err != nil {
		return nil, fmt.Errorf("failed to rename part file: %w", err)
	}

	etag := hex.EncodeToString(hash.Sum(nil))
	partInfo := PartInfo{
		PartNumber: partNumber,
		ETag:       etag,
		Size:       written,
	}

	// 2. Lock briefly at the end to update metadata
	unlock := fs.lockUpload(uploadID)
	defer unlock()

	meta, err = fs.metadata.GetMultipartMeta(bucket, key, uploadID)
	if err != nil {
		return nil, &S3Error{Code: "NoSuchUpload", Message: errUploadNotFound}
	}

	// Update metadata with part info
	found := false
	for i, p := range meta.Parts {
		if p.PartNumber == partNumber {
			meta.Parts[i] = partInfo
			found = true
			break
		}
	}
	if !found {
		meta.Parts = append(meta.Parts, partInfo)
	}

	if err := fs.metadata.PutMultipartMeta(meta); err != nil {
		return nil, err
	}

	GlobalMetrics.AddUploaded(written)

	return &partInfo, nil
}

func (fs *FilesystemEngine) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletePart) (*ObjectInfo, error) {
	if hasPathTraversal(key) || hasPathTraversal(bucket) || hasPathTraversal(uploadID) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidKeyTraversal}
	}
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, err
	}

	unlock := fs.lockUpload(uploadID)
	defer unlock()

	meta, err := fs.metadata.GetMultipartMeta(bucket, key, uploadID)
	if err != nil {
		return nil, &S3Error{Code: "NoSuchUpload", Message: errUploadNotFound}
	}

	// Because the customer key is never persisted, an SSE-C upload can only be
	// assembled if the caller re-presents it here. This is a deliberate
	// deviation from S3, which does not require SSE-C headers on Complete:
	// Stiva physically concatenates parts, so it must decrypt and re-encrypt.
	ssecParams := extractSSECParams(ctx)
	if meta.SSECustomerAlgorithm != "" {
		if ssecParams == nil {
			return nil, &S3Error{
				Code:    "InvalidArgument",
				Message: "This multipart upload used SSE-C; the customer key must be supplied to complete it",
			}
		}
		if subtle.ConstantTimeCompare([]byte(ssecParams.KeyMD5), []byte(meta.SSECustomerKeyMD5)) != 1 {
			return nil, &S3Error{Code: "InvalidDigest", Message: "The customer-provided encryption key MD5 does not match"}
		}
	} else if ssecParams != nil {
		return nil, &S3Error{
			Code:    "InvalidArgument",
			Message: "This multipart upload was not created with SSE-C and cannot be completed with a customer key",
		}
	}

	// Sort requested parts by part number and reject duplicates: two entries
	// with the same number would otherwise be concatenated twice.
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})
	for i := 1; i < len(parts); i++ {
		if parts[i].PartNumber == parts[i-1].PartNumber {
			return nil, &S3Error{
				Code:    "InvalidPartOrder",
				Message: fmt.Sprintf("Part %d is listed more than once.", parts[i].PartNumber),
			}
		}
	}
	if len(parts) == 0 {
		return nil, &S3Error{Code: "InvalidPart", Message: "You must specify at least one part."}
	}

	// Create a map for quick lookup of uploaded parts
	uploadedParts := make(map[int]PartInfo)
	for _, p := range meta.Parts {
		uploadedParts[p.PartNumber] = p
	}

	for i, part := range parts {
		uPart, exists := uploadedParts[part.PartNumber]
		if !exists {
			return nil, &S3Error{Code: "InvalidPart", Message: fmt.Sprintf("One or more of the specified parts could not be found. Part %d was not uploaded.", part.PartNumber)}
		}

		uETag := strings.Trim(uPart.ETag, `"`)
		reqETag := strings.Trim(part.ETag, `"`)
		if uETag != reqETag {
			return nil, &S3Error{Code: "InvalidPart", Message: fmt.Sprintf("ETag mismatch for part %d. Expected %s, got %s.", part.PartNumber, uETag, reqETag)}
		}

		// Enforce minimum part size of 5MB for all parts except the last one
		if !fs.disableMinPartSize && i < len(parts)-1 {
			if uPart.Size < 5*1024*1024 {
				return nil, &S3Error{
					Code:    "EntityTooSmall",
					Message: fmt.Sprintf("Your proposed upload is smaller than the minimum allowed size. Each part must be at least 5 MB in size, except the last part. Part %d is %d bytes.", part.PartNumber, uPart.Size),
				}
			}
		}
	}

	versionStatus, err := fs.metadata.GetBucketVersioning(bucket)
	if err != nil {
		versionStatus = ""
	}

	var versionID string
	switch versionStatus {
	case "Enabled":
		versionID = uuid.New().String()
	case "Suspended":
		versionID = "null"
	}

	objPath, err := fs.objectPathWithVersion(bucket, key, versionID)
	if err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}

	bucketDir := filepath.Clean(fs.bucketPath(bucket))
	bucketPrefix := bucketDir + string(filepath.Separator)
	if !strings.HasPrefix(objPath, bucketPrefix) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidKeyTraversal}
	}
	rel, err := filepath.Rel(bucketDir, objPath)
	if err != nil || filepath.IsAbs(rel) || isRelTraversal(rel) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidKeyTraversal}
	}
	objDir := filepath.Dir(objPath)
	if objDir != bucketDir && !strings.HasPrefix(objDir, bucketPrefix) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidKeyTraversal}
	}
	relDir, err := filepath.Rel(bucketDir, objDir)
	if err != nil || filepath.IsAbs(relDir) || isRelTraversal(relDir) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidKeyTraversal}
	}

	// Check for flat namespace directory/file path conflicts
	if err := fs.checkPathConflict(objPath, bucket); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(objDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create object directory: %w", err)
	}

	// Use atomic temp-file + rename for crash safety
	tmpFile, err := os.CreateTemp(objDir, ".stiva-multipart-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()

	var out io.Writer = tmpFile
	var gzipWriter *gzip.Writer

	if meta.SSECustomerAlgorithm != "" {
		newIV := make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, newIV); err != nil {
			return nil, fmt.Errorf(errFailedGenerateIV, err)
		}
		if _, err := tmpFile.Write(newIV); err != nil {
			return nil, fmt.Errorf("failed to write IV: %w", err)
		}

		block, err := aes.NewCipher(ssecParams.Key)
		if err != nil {
			return nil, fmt.Errorf(errFailedCreateAES, err)
		}
		stream := cipher.NewCTR(block, newIV)
		out = &cipher.StreamWriter{S: stream, W: out}
	}

	compressed := isCompressibleContentType(meta.ContentType)
	if compressed {
		gw := gzipWriterPool.Get().(*gzip.Writer)
		gw.Reset(out)
		gzipWriter = gw
		out = gw
	}

	hash := md5.New()
	var totalSize int64
	partDir, err := fs.multipartDir(bucket, key, uploadID)
	if err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	partBaseDir := filepath.Clean(filepath.Join(fs.dataDir, "multipart", bucket))
	partBasePrefix := partBaseDir + string(filepath.Separator)
	if !strings.HasPrefix(partDir, partBasePrefix) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidUploadID}
	}

	for _, part := range parts {
		partPath := filepath.Join(partDir, fmt.Sprintf("part-%05d", part.PartNumber))
		if !strings.HasPrefix(partPath, partBasePrefix) {
			return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidUploadID}
		}
		partFile, err := os.Open(partPath)
		if err != nil {
			return nil, fmt.Errorf("part %d not found: %w", part.PartNumber, err)
		}

		var partReader io.Reader = partFile
		var closers []io.Closer
		closers = append(closers, partFile)

		if meta.SSECustomerAlgorithm != "" {
			partIV := make([]byte, 16)
			if _, err := io.ReadFull(partFile, partIV); err != nil {
				for _, c := range closers {
					c.Close()
				}
				return nil, fmt.Errorf("failed to read part IV: %w", err)
			}
			block, err := aes.NewCipher(ssecParams.Key)
			if err != nil {
				for _, c := range closers {
					c.Close()
				}
				return nil, fmt.Errorf(errFailedCreateAES, err)
			}
			stream := cipher.NewCTR(block, partIV)
			partReader = &cipher.StreamReader{S: stream, R: partFile}
		}

		n, err := io.Copy(io.MultiWriter(out, hash), partReader)
		for _, c := range closers {
			c.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("failed to copy part %d: %w", part.PartNumber, err)
		}
		totalSize += n
	}

	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			return nil, fmt.Errorf("failed to close gzip writer: %w", err)
		}
		gzipWriterPool.Put(gzipWriter)
	}

	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	// Atomically move temp file to final location
	if !strings.HasPrefix(objPath, bucketPrefix) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidKeyTraversal}
	}
	if err := os.Rename(tmpPath, objPath); err != nil {
		return nil, fmt.Errorf("failed to rename temp file: %w", err)
	}

	// Calculate S3-compliant multipart ETag
	var md5s []byte
	for _, part := range parts {
		trimmedETag := strings.Trim(part.ETag, `"`)
		b, err := hex.DecodeString(trimmedETag)
		if err == nil && len(b) == 16 {
			md5s = append(md5s, b...)
		}
	}
	var etag string
	if len(md5s) > 0 {
		mHash := md5.New()
		_, _ = mHash.Write(md5s)
		etag = fmt.Sprintf("%s-%d", hex.EncodeToString(mHash.Sum(nil)), len(parts))
	} else {
		etag = hex.EncodeToString(hash.Sum(nil))
	}

	contentType := meta.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	info := &ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		Size:         totalSize,
		ETag:         etag,
		ContentType:  contentType,
		LastModified: time.Now().UTC(),
		VersionID:    versionID,
		Compressed:   compressed,
	}

	if meta.SSECustomerAlgorithm != "" {
		info.SSECustomerAlgorithm = meta.SSECustomerAlgorithm
		info.SSECustomerKeyMD5 = meta.SSECustomerKeyMD5
	}

	if err := fs.metadata.PutObjectMeta(info); err != nil {
		return nil, err
	}

	// Clean up multipart temp files
	if uploadPath, err := fs.multipartUploadPath(bucket, uploadID); err == nil {
		os.RemoveAll(uploadPath)
	}
	if err := fs.metadata.DeleteMultipartMeta(bucket, key, uploadID); err != nil {
		slog.Error("Failed to delete multipart metadata", "error", err, "bucket", bucket, "key", key, "uploadID", uploadID)
	}

	GlobalMetrics.DecActiveMultiparts()

	fs.triggerWebhook("ObjectCreated:CompleteMultipartUpload", info)

	fs.MirrorSync(bucket, key, "PUT")

	return info, nil
}

func (fs *FilesystemEngine) AbortMultipartUpload(bucket, key, uploadID string) error {
	if err := fs.validateBucketName(bucket); err != nil {
		return err
	}

	if _, err := fs.objectPath(bucket, key); err != nil {
		return &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}

	unlock := fs.lockUpload(uploadID)
	defer unlock()

	_, err := fs.metadata.GetMultipartMeta(bucket, key, uploadID)
	if err != nil {
		return &S3Error{Code: "NoSuchUpload", Message: errUploadNotFound}
	}

	uploadPath, err := fs.multipartUploadPath(bucket, uploadID)
	if err != nil {
		return &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	os.RemoveAll(uploadPath)
	err = fs.metadata.DeleteMultipartMeta(bucket, key, uploadID)
	if err == nil {
		GlobalMetrics.DecActiveMultiparts()
	}
	return err
}

// lockKey acquires a refcounted mutex scoped to an arbitrary string key,
// creating it on first use and removing it once the last holder releases it.
// Callers from different lock domains (uploads, parts, object keys) must
// prefix their keys so they can't collide in the shared map.
func (fs *FilesystemEngine) lockKey(key string) func() {
	fs.mu.Lock()
	if fs.locks == nil {
		fs.locks = make(map[string]*uploadLock)
	}
	l, exists := fs.locks[key]
	if !exists {
		l = &uploadLock{}
		fs.locks[key] = l
	}
	l.refCount++
	fs.mu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		fs.mu.Lock()
		l.refCount--
		if l.refCount == 0 {
			delete(fs.locks, key)
		}
		fs.mu.Unlock()
	}
}

func (fs *FilesystemEngine) lockUpload(uploadID string) func() {
	return fs.lockKey("upload:" + uploadID)
}

// lockPart serializes the write-rename-metadata sequence for a single part
// of a single upload, so a retried/duplicate UploadPart call for the same
// part number can't leave the on-disk part file from one request paired with
// the ETag/size metadata from another. Different part numbers (the common
// case for a parallel multipart upload) are unaffected by each other.
func (fs *FilesystemEngine) lockPart(uploadID string, partNumber int) func() {
	return fs.lockKey(fmt.Sprintf("part:%s:%d", uploadID, partNumber))
}

// lockObjectKey serializes the rename-then-metadata-write sequence in
// PutObject for a single (bucket, key), so two racing PUTs to the same
// unversioned key can't leave the on-disk bytes from one request paired with
// the ETag/size metadata from the other.
func (fs *FilesystemEngine) lockObjectKey(bucket, key string) func() {
	return fs.lockKey("object:" + bucket + "/" + key)
}
