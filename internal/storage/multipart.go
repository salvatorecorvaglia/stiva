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
	"sync"
	"time"

	"github.com/google/uuid"
)

func (fs *FilesystemEngine) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (*MultipartUploadInfo, error) {
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, err
	}

	if _, err := fs.objectPath(bucket, key); err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	if err := validateNewKey(key); err != nil {
		return nil, err
	}

	if err := fs.bucketMustExist(bucket); err != nil {
		return nil, err
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

	if err := syncParents(partDir, fs.dataDir); err != nil {
		return nil, err
	}
	meta := &MultipartMeta{
		UploadID:    uploadID,
		Bucket:      bucket,
		Key:         key,
		ContentType: contentType,
		Created:     time.Now().UTC(),
	}
	// The SSE-C mode is fixed here, once. Only the algorithm and key MD5 are
	// kept; the key itself is never stored (see MultipartMeta).
	if params := extractSSECParams(ctx); params != nil {
		meta.SSECustomerAlgorithm = params.Algorithm
		meta.SSECustomerKeyMD5 = params.KeyMD5
	}

	// Recorded under the shared bucket lock so DeleteBucket, which refuses to
	// remove a bucket with uploads in progress, can't miss this one.
	unlockBucket, err := fs.lockBucketForCommit(bucket)
	if err != nil {
		return nil, err
	}
	defer unlockBucket()

	if err := fs.metadata.PutMultipartMeta(meta); err != nil {
		return nil, err
	}

	fs.Metrics().IncActiveMultiparts()

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

	ssecParams := extractSSECParams(ctx)

	meta, err := fs.metadata.GetMultipartMeta(bucket, key, uploadID)
	if err != nil {
		return nil, &S3Error{Code: "NoSuchUpload", Message: errUploadNotFound}
	}

	// Every part must match the SSE-C mode declared at creation. The mode used
	// to be adopted from whichever part arrived first, so parallel first parts
	// could leave one object assembled from encrypted and plaintext parts.
	if meta.SSECustomerAlgorithm != "" && ssecParams == nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: "This multipart upload was created with SSE-C; every part must supply the customer key"}
	}
	if meta.SSECustomerAlgorithm == "" && ssecParams != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: "This multipart upload was not created with SSE-C and cannot accept a customer key"}
	}
	if meta.SSECustomerAlgorithm != "" {
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
	partID, err := newBlobID()
	if err != nil {
		return nil, err
	}
	partPath, err := partDataPath(partDir, PartInfo{PartNumber: partNumber, BlobID: partID})
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(partPath)
		}
	}()

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

	if err := tmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("sync object data: %w", err)
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

	if err := syncParents(filepath.Dir(partPath), fs.dataDir); err != nil {
		return nil, err
	}
	etag := hex.EncodeToString(hash.Sum(nil))
	partInfo := PartInfo{
		BlobID:     partID,
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

	// Retain the previous immutable file until the replacement metadata commits.
	var previous *PartInfo
	// Update metadata with part info
	found := false
	for i, p := range meta.Parts {
		if p.PartNumber == partNumber {
			previous = &p
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

	committed = true
	if previous != nil {
		if oldPath, err := partDataPath(partDir, *previous); err == nil {
			removeObjectFile(oldPath)
		}
	}
	fs.Metrics().AddUploaded(written)

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

	if err := validateNewKey(key); err != nil {
		return nil, err
	}
	if _, err := fs.objectPathWithVersion(bucket, key, versionID); err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	blobID, blobPath, err := fs.newBlob(bucket)
	if err != nil {
		return nil, err
	}

	// Use atomic temp-file + rename for crash safety
	tmpFile, err := os.CreateTemp(filepath.Dir(blobPath), ".stiva-multipart-*")
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

	// The assembled length is known from the parts, so the same size rule
	// applies here as in PutObject. Multipart uploads are large by
	// construction, so in practice this leaves them seekable.
	var expectedSize int64
	for _, part := range parts {
		expectedSize += uploadedParts[part.PartNumber].Size
	}

	compressed := shouldCompress(meta.ContentType, expectedSize)
	if compressed {
		gw := gzipWriterPool.Get().(*gzip.Writer)
		gw.Reset(out)
		gzipWriter = gw
		out = gw
	}

	// The S3 multipart ETag is the MD5 of the parts' binary MD5s, suffixed
	// with the part count, so it comes from the part ETags alone. The whole
	// assembled object used to be MD5'd as well, only as a fallback that
	// could not trigger — and routing every byte through that hash is what
	// kept io.Copy below from using copy_file_range between part and object.
	etag, err := multipartETag(parts, uploadedParts)
	if err != nil {
		return nil, err
	}

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
		partPath, err := partDataPath(partDir, uploadedParts[part.PartNumber])
		if err != nil {
			return nil, err
		}
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

		n, err := io.Copy(out, partReader)
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

	if err := tmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("sync object data: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	// Same commit discipline as PutObject: the per-key lock keeps a racing
	// PUT or DELETE of this key from interleaving between the rename and the
	// metadata write, and the shared bucket lock keeps DeleteBucket out.
	unlockObj := fs.lockObjectKey(bucket, key)
	defer unlockObj()

	unlockBucket, err := fs.lockBucketForCommit(bucket)
	if err != nil {
		return nil, err
	}
	defer unlockBucket()

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

	if err := fs.commitObject(tmpPath, blobID, blobPath, info); err != nil {
		return nil, err
	}

	// Clean up multipart temp files
	if uploadPath, err := fs.multipartUploadPath(bucket, uploadID); err == nil {
		removeUploadDir(uploadPath)
	}
	if err := fs.metadata.DeleteMultipartMeta(bucket, key, uploadID); err != nil {
		slog.Error("Failed to delete multipart metadata", "error", err, "bucket", bucket, "key", key, "uploadID", uploadID)
	}

	fs.Metrics().DecActiveMultiparts()

	fs.triggerWebhook("ObjectCreated:CompleteMultipartUpload", info)

	fs.MirrorSync(bucket, key, "PUT")

	return info, nil
}

// removeUploadDir deletes a finished or aborted upload's part files, logging
// a failure instead of silently leaving the parts on disk.
func removeUploadDir(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("[Storage] Failed to remove multipart upload directory; its parts are now orphaned on disk", "path", path, "error", err)
	}
}

// multipartETag computes the S3 ETag of an assembled multipart object: the
// MD5 of the concatenated binary MD5s of its parts, then "-" and the part
// count. It uses the ETags recorded when each part was uploaded, which the
// caller has already matched against the completion request.
func multipartETag(parts []CompletePart, uploaded map[int]PartInfo) (string, error) {
	h := md5.New()
	for _, part := range parts {
		sum, err := hex.DecodeString(strings.Trim(uploaded[part.PartNumber].ETag, `"`))
		if err != nil || len(sum) != md5.Size {
			return "", fmt.Errorf("part %d has a malformed stored ETag %q", part.PartNumber, uploaded[part.PartNumber].ETag)
		}
		_, _ = h.Write(sum)
	}
	return fmt.Sprintf("%s-%d", hex.EncodeToString(h.Sum(nil)), len(parts)), nil
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
	removeUploadDir(uploadPath)
	err = fs.metadata.DeleteMultipartMeta(bucket, key, uploadID)
	if err == nil {
		fs.Metrics().DecActiveMultiparts()
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

// bucketRWLock is a refcounted read/write lock scoped to one bucket.
type bucketRWLock struct {
	sync.RWMutex
	refCount int
}

// lockBucket takes a bucket's engine-level lock: exclusive for creating or
// deleting the bucket itself, shared for committing an object into it.
//
// This is what makes DeleteBucket atomic with respect to object writes. It
// used to check the bucket was empty and then remove it as separate steps, so
// a PUT committing in between was acknowledged while its data went away with
// the bucket. A committer holds the shared lock and re-checks the bucket still
// exists; DeleteBucket holds the exclusive lock across its check and removal.
//
// It is separate from the metadata store's own bucket locks, which the calls
// made under this one take internally; sharing them would mean re-entrant
// read locking, which deadlocks against a waiting writer.
func (fs *FilesystemEngine) lockBucket(bucket string, exclusive bool) func() {
	fs.mu.Lock()
	if fs.bucketLocks == nil {
		fs.bucketLocks = make(map[string]*bucketRWLock)
	}
	l, ok := fs.bucketLocks[bucket]
	if !ok {
		l = &bucketRWLock{}
		fs.bucketLocks[bucket] = l
	}
	l.refCount++
	fs.mu.Unlock()

	if exclusive {
		l.Lock()
	} else {
		l.RLock()
	}
	return func() {
		if exclusive {
			l.Unlock()
		} else {
			l.RUnlock()
		}
		fs.mu.Lock()
		l.refCount--
		if l.refCount == 0 {
			delete(fs.bucketLocks, bucket)
		}
		fs.mu.Unlock()
	}
}

// lockBucketForCommit takes the shared bucket lock and confirms the bucket
// still exists, for the final step of a write. On success the caller must
// call the returned release function.
func (fs *FilesystemEngine) lockBucketForCommit(bucket string) (func(), error) {
	unlock := fs.lockBucket(bucket, false)
	if err := fs.bucketMustExist(bucket); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
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

// partDataPath reads both the immutable and legacy part layouts.
func partDataPath(directory string, part PartInfo) (string, error) {
	name := fmt.Sprintf("part-%05d", part.PartNumber)
	if part.BlobID != "" {
		if !isBlobID(part.BlobID) {
			return "", fmt.Errorf("invalid multipart blob ID")
		}
		name += "-" + part.BlobID
	}
	return filepath.Join(directory, name), nil
}
