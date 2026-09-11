package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// setupTestEngine creates a temporary FilesystemEngine for testing.
func setupTestEngine(t *testing.T) *FilesystemEngine {
	t.Helper()
	tmpDir := t.TempDir()
	engine, err := NewFilesystemEngine(tmpDir, nil, "")
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	// Most tests here use small multipart parts.
	engine.SetDisableMinPartSize(true)
	t.Cleanup(func() { engine.Close() })
	return engine
}

// --- Bucket Operations ---

func TestCreateBucket(t *testing.T) {
	engine := setupTestEngine(t)

	err := engine.CreateBucket("test-bucket")
	if err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}

	exists, err := engine.BucketExists("test-bucket")
	if err != nil {
		t.Fatalf("BucketExists failed: %v", err)
	}
	if !exists {
		t.Error("expected bucket to exist after creation")
	}

	// Verify directory was created
	bucketDir := filepath.Join(engine.DataDir(), "buckets", "test-bucket")
	if _, err := os.Stat(bucketDir); os.IsNotExist(err) {
		t.Error("expected bucket directory to exist on disk")
	}
}

func TestCreateBucketDuplicate(t *testing.T) {
	engine := setupTestEngine(t)

	if err := engine.CreateBucket("test-bucket"); err != nil {
		t.Fatalf("first CreateBucket failed: %v", err)
	}

	err := engine.CreateBucket("test-bucket")
	if err == nil {
		t.Error("expected error when creating duplicate bucket")
	}
	var s3Err *S3Error
	ok := errors.As(err, &s3Err)
	if !ok {
		t.Fatalf("expected S3Error, got %T", err)
	}
	if s3Err.Code != "BucketAlreadyOwnedByYou" {
		t.Errorf("expected BucketAlreadyOwnedByYou, got %s", s3Err.Code)
	}
}

func TestDeleteBucket(t *testing.T) {
	engine := setupTestEngine(t)

	if err := engine.CreateBucket("delete-me"); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}

	if err := engine.DeleteBucket("delete-me"); err != nil {
		t.Fatalf("DeleteBucket failed: %v", err)
	}

	exists, _ := engine.BucketExists("delete-me")
	if exists {
		t.Error("expected bucket to not exist after deletion")
	}
}

func TestDeleteBucketNotEmpty(t *testing.T) {
	engine := setupTestEngine(t)

	if err := engine.CreateBucket("notempty"); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}

	// Put an object in the bucket
	_, err := engine.PutObject(context.Background(), "notempty", "file.txt", bytes.NewReader([]byte("data")), 4, "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	err = engine.DeleteBucket("notempty")
	if err == nil {
		t.Error("expected error when deleting non-empty bucket")
	}
}

func TestDeleteBucketNonexistent(t *testing.T) {
	engine := setupTestEngine(t)

	err := engine.DeleteBucket("no-such-bucket")
	if err == nil {
		t.Error("expected error when deleting non-existent bucket")
	}
}

func TestListBuckets(t *testing.T) {
	engine := setupTestEngine(t)

	// Initially empty
	buckets, err := engine.ListBuckets()
	if err != nil {
		t.Fatalf("ListBuckets failed: %v", err)
	}
	if len(buckets) != 0 {
		t.Errorf("expected 0 buckets, got %d", len(buckets))
	}

	// Create some buckets
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := engine.CreateBucket(name); err != nil {
			t.Fatalf("CreateBucket(%s) failed: %v", name, err)
		}
	}

	buckets, err = engine.ListBuckets()
	if err != nil {
		t.Fatalf("ListBuckets failed: %v", err)
	}
	if len(buckets) != 3 {
		t.Errorf("expected 3 buckets, got %d", len(buckets))
	}
}

// --- Object Operations ---

func TestPutAndGetObject(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("my-bucket")

	content := []byte("Hello, Stiva!")
	info, err := engine.PutObject(context.Background(), "my-bucket", "greeting.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	if info.Key != "greeting.txt" {
		t.Errorf("expected key 'greeting.txt', got '%s'", info.Key)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), info.Size)
	}
	if info.ContentType != "text/plain" {
		t.Errorf("expected content type 'text/plain', got '%s'", info.ContentType)
	}
	if info.ETag == "" {
		t.Error("expected non-empty ETag")
	}

	// Get it back
	reader, getInfo, err := engine.GetObject(context.Background(), "my-bucket", "greeting.txt", "")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read object: %v", err)
	}

	if !bytes.Equal(data, content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}
	if getInfo.Size != int64(len(content)) {
		t.Errorf("size mismatch: got %d, want %d", getInfo.Size, len(content))
	}
}

func TestPutObjectNoBucket(t *testing.T) {
	engine := setupTestEngine(t)

	_, err := engine.PutObject(context.Background(), "nonexistent", "file.txt", bytes.NewReader([]byte("data")), 4, "text/plain")
	if err == nil {
		t.Error("expected error when putting object to non-existent bucket")
	}
}

func TestGetObjectNotFound(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("my-bucket")

	_, _, err := engine.GetObject(context.Background(), "my-bucket", "no-such-key", "")
	if err == nil {
		t.Error("expected error when getting non-existent object")
	}
	var s3Err *S3Error
	ok := errors.As(err, &s3Err)
	if !ok {
		t.Fatalf("expected S3Error, got %T", err)
	}
	if s3Err.Code != "NoSuchKey" {
		t.Errorf("expected NoSuchKey, got %s", s3Err.Code)
	}
}

func TestHeadObject(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("bucket")

	content := []byte("metadata test")
	engine.PutObject(context.Background(), "bucket", "doc.txt", bytes.NewReader(content), int64(len(content)), "text/plain")

	info, err := engine.HeadObject(context.Background(), "bucket", "doc.txt", "")
	if err != nil {
		t.Fatalf("HeadObject failed: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), info.Size)
	}
	if info.ContentType != "text/plain" {
		t.Errorf("expected content type 'text/plain', got '%s'", info.ContentType)
	}
}

func TestDeleteObject(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("bucket")
	engine.PutObject(context.Background(), "bucket", "deleteme.txt", bytes.NewReader([]byte("bye")), 3, "text/plain")

	_, _, err := engine.DeleteObject("bucket", "deleteme.txt", "")
	if err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	_, _, err = engine.GetObject(context.Background(), "bucket", "deleteme.txt", "")
	if err == nil {
		t.Error("expected error when getting deleted object")
	}
}

func TestCopyObject(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("src-bucket")
	engine.CreateBucket("dst-bucket")

	content := []byte("copy this content")
	engine.PutObject(context.Background(), "src-bucket", "original.txt", bytes.NewReader(content), int64(len(content)), "text/plain")

	info, err := engine.CopyObject(context.Background(),
		CopySource{Bucket: "src-bucket", Key: "original.txt"}, "dst-bucket", "copy.txt")
	if err != nil {
		t.Fatalf("CopyObject failed: %v", err)
	}
	if info.Key != "copy.txt" {
		t.Errorf("expected key 'copy.txt', got '%s'", info.Key)
	}

	// Verify the copy
	reader, _, err := engine.GetObject(context.Background(), "dst-bucket", "copy.txt", "")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if !bytes.Equal(data, content) {
		t.Errorf("copied content mismatch")
	}
}

func TestNestedKeyPaths(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("bucket")

	content := []byte("nested file")
	_, err := engine.PutObject(context.Background(), "bucket", "photos/2024/01/img.jpg", bytes.NewReader(content), int64(len(content)), "image/jpeg")
	if err != nil {
		t.Fatalf("PutObject with nested key failed: %v", err)
	}

	reader, _, err := engine.GetObject(context.Background(), "bucket", "photos/2024/01/img.jpg", "")
	if err != nil {
		t.Fatalf("GetObject with nested key failed: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if !bytes.Equal(data, content) {
		t.Error("nested object content mismatch")
	}
}

// --- Path Traversal Protection ---

func TestPathTraversalProtection(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("bucket")

	_, err := engine.PutObject(context.Background(), "bucket", "../../etc/passwd", bytes.NewReader([]byte("malicious")), 9, "text/plain")
	if err == nil {
		t.Error("expected error for path traversal key")
	}
}

// --- List Operations ---

func TestListObjects(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("list-bucket")

	// Create several objects
	objects := []string{"a.txt", "b.txt", "c.txt", "dir/d.txt", "dir/e.txt"}
	for _, key := range objects {
		engine.PutObject(context.Background(), "list-bucket", key, bytes.NewReader([]byte("x")), 1, "text/plain")
	}

	// List all
	output, err := engine.ListObjects(&ListObjectsInput{
		Bucket:  "list-bucket",
		MaxKeys: 1000,
	})
	if err != nil {
		t.Fatalf("ListObjects failed: %v", err)
	}
	if len(output.Objects) != 5 {
		t.Errorf("expected 5 objects, got %d", len(output.Objects))
	}
}

func TestListObjectsWithDelimiter(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("delim-bucket")

	keys := []string{"a.txt", "b.txt", "dir/c.txt", "dir/d.txt", "other/e.txt"}
	for _, key := range keys {
		engine.PutObject(context.Background(), "delim-bucket", key, bytes.NewReader([]byte("x")), 1, "text/plain")
	}

	output, err := engine.ListObjects(&ListObjectsInput{
		Bucket:    "delim-bucket",
		Delimiter: "/",
		MaxKeys:   1000,
	})
	if err != nil {
		t.Fatalf("ListObjects failed: %v", err)
	}

	// Should have 2 objects (a.txt, b.txt) and 2 common prefixes (dir/, other/)
	if len(output.Objects) != 2 {
		t.Errorf("expected 2 objects, got %d", len(output.Objects))
	}
	if len(output.CommonPrefixes) != 2 {
		t.Errorf("expected 2 common prefixes, got %d: %v", len(output.CommonPrefixes), output.CommonPrefixes)
	}
}

func TestListObjectsPagination(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("page-bucket")

	// Create 5 objects
	for i := 0; i < 5; i++ {
		key := string(rune('a'+i)) + ".txt"
		engine.PutObject(context.Background(), "page-bucket", key, bytes.NewReader([]byte("x")), 1, "text/plain")
	}

	// Paginate with maxKeys=2
	output, err := engine.ListObjects(&ListObjectsInput{
		Bucket:  "page-bucket",
		MaxKeys: 2,
	})
	if err != nil {
		t.Fatalf("ListObjects page 1 failed: %v", err)
	}
	if len(output.Objects) != 2 {
		t.Errorf("page 1: expected 2 objects, got %d", len(output.Objects))
	}
	if !output.IsTruncated {
		t.Error("page 1: expected IsTruncated=true")
	}
	if output.NextContinuationToken == "" {
		t.Error("page 1: expected NextContinuationToken to be set")
	}

	// Get page 2
	output2, err := engine.ListObjects(&ListObjectsInput{
		Bucket:            "page-bucket",
		MaxKeys:           2,
		ContinuationToken: output.NextContinuationToken,
	})
	if err != nil {
		t.Fatalf("ListObjects page 2 failed: %v", err)
	}
	if len(output2.Objects) != 2 {
		t.Errorf("page 2: expected 2 objects, got %d", len(output2.Objects))
	}

	// Get page 3 (last page)
	output3, err := engine.ListObjects(&ListObjectsInput{
		Bucket:            "page-bucket",
		MaxKeys:           2,
		ContinuationToken: output2.NextContinuationToken,
	})
	if err != nil {
		t.Fatalf("ListObjects page 3 failed: %v", err)
	}
	if len(output3.Objects) != 1 {
		t.Errorf("page 3: expected 1 object, got %d", len(output3.Objects))
	}
	if output3.IsTruncated {
		t.Error("page 3: expected IsTruncated=false")
	}
}

func TestCountObjects(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("count-bucket")

	count, err := engine.CountObjects("count-bucket")
	if err != nil {
		t.Fatalf("CountObjects failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// Add objects
	for i := 0; i < 10; i++ {
		key := string(rune('a'+i)) + ".txt"
		engine.PutObject(context.Background(), "count-bucket", key, bytes.NewReader([]byte("x")), 1, "text/plain")
	}

	count, err = engine.CountObjects("count-bucket")
	if err != nil {
		t.Fatalf("CountObjects failed: %v", err)
	}
	if count != 10 {
		t.Errorf("expected 10, got %d", count)
	}
}

// --- Multipart Upload ---

func TestMultipartUpload(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("multi-bucket")

	// Create multipart upload
	upload, err := engine.CreateMultipartUpload("multi-bucket", "large-file.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}
	if upload.UploadID == "" {
		t.Error("expected non-empty UploadID")
	}

	// Upload 3 parts
	partData := [][]byte{
		bytes.Repeat([]byte("A"), 100),
		bytes.Repeat([]byte("B"), 200),
		bytes.Repeat([]byte("C"), 150),
	}

	var completeParts []CompletePart
	for i, data := range partData {
		partInfo, err := engine.UploadPart(context.Background(), "multi-bucket", "large-file.bin", upload.UploadID, i+1, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("UploadPart %d failed: %v", i+1, err)
		}
		completeParts = append(completeParts, CompletePart{
			PartNumber: i + 1,
			ETag:       partInfo.ETag,
		})
	}

	// Complete the upload
	info, err := engine.CompleteMultipartUpload(context.Background(), "multi-bucket", "large-file.bin", upload.UploadID, completeParts)
	if err != nil {
		t.Fatalf("CompleteMultipartUpload failed: %v", err)
	}

	expectedSize := int64(100 + 200 + 150)
	if info.Size != expectedSize {
		t.Errorf("expected size %d, got %d", expectedSize, info.Size)
	}

	// Verify content
	reader, _, err := engine.GetObject(context.Background(), "multi-bucket", "large-file.bin", "")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if int64(len(data)) != expectedSize {
		t.Errorf("content length mismatch: got %d, want %d", len(data), expectedSize)
	}
}

func TestAbortMultipartUpload(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("abort-bucket")

	upload, err := engine.CreateMultipartUpload("abort-bucket", "aborted.txt", "text/plain")
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	// Upload a part
	engine.UploadPart(context.Background(), "abort-bucket", "aborted.txt", upload.UploadID, 1, bytes.NewReader([]byte("data")), 4)

	// Abort
	err = engine.AbortMultipartUpload("abort-bucket", "aborted.txt", upload.UploadID)
	if err != nil {
		t.Fatalf("AbortMultipartUpload failed: %v", err)
	}

	// Verify object doesn't exist
	_, _, err = engine.GetObject(context.Background(), "abort-bucket", "aborted.txt", "")
	if err == nil {
		t.Error("expected error: aborted upload should not create an object")
	}
}

// --- Unicode Keys ---

func TestUnicodeKeys(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("unicode-bucket")

	keys := []string{
		"café/résumé.pdf",
		"日本語/ファイル.txt",
		"emoji/🎉.txt",
	}

	for _, key := range keys {
		_, err := engine.PutObject(context.Background(), "unicode-bucket", key, bytes.NewReader([]byte("unicode")), 7, "text/plain")
		if err != nil {
			t.Errorf("PutObject(%q) failed: %v", key, err)
			continue
		}

		reader, _, err := engine.GetObject(context.Background(), "unicode-bucket", key, "")
		if err != nil {
			t.Errorf("GetObject(%q) failed: %v", key, err)
			continue
		}
		reader.Close()
	}
}

// --- Empty Object ---

func TestEmptyObject(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("empty-bucket")

	info, err := engine.PutObject(context.Background(), "empty-bucket", "empty.txt", bytes.NewReader([]byte{}), 0, "text/plain")
	if err != nil {
		t.Fatalf("PutObject empty failed: %v", err)
	}
	if info.Size != 0 {
		t.Errorf("expected size 0, got %d", info.Size)
	}

	reader, _, err := engine.GetObject(context.Background(), "empty-bucket", "empty.txt", "")
	if err != nil {
		t.Fatalf("GetObject empty failed: %v", err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if len(data) != 0 {
		t.Errorf("expected empty data, got %d bytes", len(data))
	}
}

// --- Overwrite Object ---

func TestOverwriteObject(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("overwrite-bucket")

	// Write v1
	engine.PutObject(context.Background(), "overwrite-bucket", "file.txt", bytes.NewReader([]byte("version1")), 8, "text/plain")

	// Overwrite with v2
	info, err := engine.PutObject(context.Background(), "overwrite-bucket", "file.txt", bytes.NewReader([]byte("version2-longer")), 15, "text/plain")
	if err != nil {
		t.Fatalf("PutObject overwrite failed: %v", err)
	}
	if info.Size != 15 {
		t.Errorf("expected size 15, got %d", info.Size)
	}

	// Verify content is v2
	reader, _, _ := engine.GetObject(context.Background(), "overwrite-bucket", "file.txt", "")
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "version2-longer" {
		t.Errorf("expected 'version2-longer', got %q", data)
	}
}

func TestBucketNameTraversalProtection(t *testing.T) {
	engine := setupTestEngine(t)

	invalidBuckets := []string{"..", "../etc", "my/bucket", "my bucket", "a"}
	for _, invalid := range invalidBuckets {
		if err := engine.CreateBucket(invalid); err == nil {
			t.Errorf("expected error when creating invalid bucket name %q", invalid)
		}
		if _, err := engine.BucketExists(invalid); err == nil {
			t.Errorf("expected error when checking exists of invalid bucket name %q", invalid)
		}
		if _, err := engine.PutObject(context.Background(), invalid, "test.txt", bytes.NewReader([]byte("x")), 1, "text/plain"); err == nil {
			t.Errorf("expected error when putting object in invalid bucket name %q", invalid)
		}
		if _, _, err := engine.GetObject(context.Background(), invalid, "test.txt", ""); err == nil {
			t.Errorf("expected error when getting object from invalid bucket name %q", invalid)
		}
		if _, _, err := engine.DeleteObject(invalid, "test.txt", ""); err == nil {
			t.Errorf("expected error when deleting object from invalid bucket name %q", invalid)
		}
	}
}

func TestConcurrentUploadPartLocking(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("concurrency-bucket")

	upload, err := engine.CreateMultipartUpload("concurrency-bucket", "concurrent.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	const numParts = 20
	var wg sync.WaitGroup
	wg.Add(numParts)

	for i := 1; i <= numParts; i++ {
		go func(partNum int) {
			defer wg.Done()
			partData := []byte(fmt.Sprintf("part data %d", partNum))
			_, uploadErr := engine.UploadPart(context.Background(), "concurrency-bucket", "concurrent.bin", upload.UploadID, partNum, bytes.NewReader(partData), int64(len(partData)))
			if uploadErr != nil {
				t.Errorf("UploadPart %d failed: %v", partNum, uploadErr)
			}
		}(i)
	}

	wg.Wait()

	// Retrieve the multipart metadata and check that all parts are present
	meta, err := engine.Metadata().GetMultipartMeta("concurrency-bucket", "concurrent.bin", upload.UploadID)
	if err != nil {
		t.Fatalf("GetMultipartMeta failed: %v", err)
	}

	if len(meta.Parts) != numParts {
		t.Errorf("expected %d parts in metadata, got %d (metadata loss occurred!)", numParts, len(meta.Parts))
	}
}

func TestMultipartUploadETagFormat(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("etag-bucket")

	upload, err := engine.CreateMultipartUpload("etag-bucket", "file.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	p1Data := []byte("part 1 data")
	p2Data := []byte("part 2 data")

	p1, err := engine.UploadPart(context.Background(), "etag-bucket", "file.bin", upload.UploadID, 1, bytes.NewReader(p1Data), int64(len(p1Data)))
	if err != nil {
		t.Fatalf("UploadPart 1 failed: %v", err)
	}

	p2, err := engine.UploadPart(context.Background(), "etag-bucket", "file.bin", upload.UploadID, 2, bytes.NewReader(p2Data), int64(len(p2Data)))
	if err != nil {
		t.Fatalf("UploadPart 2 failed: %v", err)
	}

	// Concatenate parts' raw binary MD5s
	h1, _ := hex.DecodeString(p1.ETag)
	h2, _ := hex.DecodeString(p2.ETag)
	concat := make([]byte, 0, len(h1)+len(h2))
	concat = append(concat, h1...)
	concat = append(concat, h2...)
	hFinal := md5.Sum(concat)
	expectedETag := fmt.Sprintf("%s-2", hex.EncodeToString(hFinal[:]))

	parts := []CompletePart{
		{PartNumber: 1, ETag: p1.ETag},
		{PartNumber: 2, ETag: p2.ETag},
	}

	info, err := engine.CompleteMultipartUpload(context.Background(), "etag-bucket", "file.bin", upload.UploadID, parts)
	if err != nil {
		t.Fatalf("CompleteMultipartUpload failed: %v", err)
	}

	if info.ETag != expectedETag {
		t.Errorf("expected completed ETag %q, got %q", expectedETag, info.ETag)
	}
}

func TestCleanExpiredMultipartUploads(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("cleanup-bucket")

	upload, err := engine.CreateMultipartUpload("cleanup-bucket", "file.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	partData := []byte("some part data")
	_, err = engine.UploadPart(context.Background(), "cleanup-bucket", "file.bin", upload.UploadID, 1, bytes.NewReader(partData), int64(len(partData)))
	if err != nil {
		t.Fatalf("UploadPart failed: %v", err)
	}

	// Verify temp directory exists
	partDir, err := engine.multipartDir("cleanup-bucket", "file.bin", upload.UploadID)
	if err != nil {
		t.Fatalf("multipartDir failed: %v", err)
	}
	if _, err := os.Stat(partDir); os.IsNotExist(err) {
		t.Fatal("expected multipart directory to exist")
	}

	// Manually set creation date in metadata to be 25 hours ago
	meta, err := engine.Metadata().GetMultipartMeta("cleanup-bucket", "file.bin", upload.UploadID)
	if err != nil {
		t.Fatalf("GetMultipartMeta failed: %v", err)
	}
	meta.Created = time.Now().UTC().Add(-25 * time.Hour)
	err = engine.Metadata().PutMultipartMeta(meta)
	if err != nil {
		t.Fatalf("PutMultipartMeta failed: %v", err)
	}

	// Run cleanup
	err = engine.CleanExpiredMultipartUploads(24 * time.Hour)
	if err != nil {
		t.Fatalf("CleanExpiredMultipartUploads failed: %v", err)
	}

	// Verify temp directory is deleted
	if _, err := os.Stat(partDir); !os.IsNotExist(err) {
		t.Error("expected multipart directory to be deleted")
	}

	// Verify metadata is deleted
	_, err = engine.Metadata().GetMultipartMeta("cleanup-bucket", "file.bin", upload.UploadID)
	if err == nil {
		t.Error("expected metadata to be deleted from database")
	}
}

func TestObjectVersioning(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "version-bucket"
	engine.CreateBucket(bucket)

	// Default status should be disabled (empty)
	status, err := engine.GetBucketVersioning(bucket)
	if err != nil {
		t.Fatalf("GetBucketVersioning failed: %v", err)
	}
	if status != "" {
		t.Errorf("expected empty default status, got %q", status)
	}

	// Set versioning to Enabled
	err = engine.SetBucketVersioning(bucket, "Enabled")
	if err != nil {
		t.Fatalf("SetBucketVersioning failed: %v", err)
	}
	status, err = engine.GetBucketVersioning(bucket)
	if err != nil || status != "Enabled" {
		t.Fatalf("expected status Enabled, got %q (err=%v)", status, err)
	}

	// Put version 1
	info1, err := engine.PutObject(context.Background(), bucket, "file.txt", bytes.NewReader([]byte("v1")), 2, "text/plain")
	if err != nil {
		t.Fatalf("PutObject v1 failed: %v", err)
	}
	if info1.VersionID == "" {
		t.Error("expected non-empty version ID for v1")
	}

	// Put version 2
	info2, err := engine.PutObject(context.Background(), bucket, "file.txt", bytes.NewReader([]byte("version2")), 8, "text/plain")
	if err != nil {
		t.Fatalf("PutObject v2 failed: %v", err)
	}
	if info2.VersionID == "" {
		t.Error("expected non-empty version ID for v2")
	}
	if info1.VersionID == info2.VersionID {
		t.Error("expected version IDs to be different")
	}

	// Get latest version (should be v2)
	reader, getInfo, err := engine.GetObject(context.Background(), bucket, "file.txt", "")
	if err != nil {
		t.Fatalf("GetObject latest failed: %v", err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if string(data) != "version2" {
		t.Errorf("expected 'version2', got %q", data)
	}
	if getInfo.VersionID != info2.VersionID {
		t.Errorf("expected version ID %s, got %s", info2.VersionID, getInfo.VersionID)
	}

	// Get v1 specifically
	reader1, getInfo1, err := engine.GetObject(context.Background(), bucket, "file.txt", info1.VersionID)
	if err != nil {
		t.Fatalf("GetObject v1 failed: %v", err)
	}
	data1, _ := io.ReadAll(reader1)
	reader1.Close()
	if string(data1) != "v1" {
		t.Errorf("expected 'v1', got %q", data1)
	}
	if getInfo1.VersionID != info1.VersionID {
		t.Errorf("expected version ID %s, got %s", info1.VersionID, getInfo1.VersionID)
	}

	// Head v1 and v2
	h1, err := engine.HeadObject(context.Background(), bucket, "file.txt", info1.VersionID)
	if err != nil || h1.Size != 2 {
		t.Errorf("HeadObject v1 failed: %v", err)
	}
	h2, err := engine.HeadObject(context.Background(), bucket, "file.txt", info2.VersionID)
	if err != nil || h2.Size != 8 {
		t.Errorf("HeadObject v2 failed: %v", err)
	}

	// Delete without version ID should create a delete marker
	_, _, err = engine.DeleteObject(bucket, "file.txt", "")
	if err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Head latest version should now return NoSuchKey
	_, err = engine.HeadObject(context.Background(), bucket, "file.txt", "")
	if err == nil {
		t.Error("expected NoSuchKey error for head after deletion")
	}

	// Get latest version should return NoSuchKey
	_, _, err = engine.GetObject(context.Background(), bucket, "file.txt", "")
	if err == nil {
		t.Error("expected NoSuchKey error for get after deletion")
	}

	// Retrieve v2 specifically (should still be available)
	reader2, _, err := engine.GetObject(context.Background(), bucket, "file.txt", info2.VersionID)
	if err != nil {
		t.Fatalf("GetObject v2 after deletion failed: %v", err)
	}
	reader2.Close()

	// Permanently delete v2
	_, _, err = engine.DeleteObject(bucket, "file.txt", info2.VersionID)
	if err != nil {
		t.Fatalf("DeleteObject v2 failed: %v", err)
	}

	// Retrieve v2 specifically should now fail
	_, _, err = engine.GetObject(context.Background(), bucket, "file.txt", info2.VersionID)
	if err == nil {
		t.Error("expected error retrieving permanently deleted v2")
	}
}

func TestPublicBuckets(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "policy-bucket"
	err := engine.CreateBucket(bucket)
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	// Default should not be public
	pub, err := engine.IsBucketPublic(bucket)
	if err != nil {
		t.Fatalf("failed to check public state: %v", err)
	}
	if pub {
		t.Error("expected default public state to be false")
	}

	// Toggle public state
	err = engine.SetBucketPublic(bucket, true)
	if err != nil {
		t.Fatalf("failed to set public state: %v", err)
	}

	pub, err = engine.IsBucketPublic(bucket)
	if err != nil {
		t.Fatalf("failed to check public state: %v", err)
	}
	if !pub {
		t.Error("expected public state to be true")
	}
}

func TestSSEC(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "ssec-bucket"
	err := engine.CreateBucket(bucket)
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	key := []byte("01234567890123456789012345678901") // 32 bytes
	// MD5 of key
	hash := md5.Sum(key)
	keyMD5 := base64.StdEncoding.EncodeToString(hash[:])

	params := &SSECParams{
		Algorithm: "AES256",
		Key:       key,
		KeyMD5:    keyMD5,
	}

	ctx := context.WithValue(context.Background(), SSECContextKey, params)

	content := []byte("secret payload")
	info, err := engine.PutObject(ctx, bucket, "secret.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject with SSE-C failed: %v", err)
	}

	if info.SSECustomerAlgorithm != "AES256" {
		t.Errorf("expected SSECustomerAlgorithm AES256, got %q", info.SSECustomerAlgorithm)
	}
	if info.SSECustomerKeyMD5 != keyMD5 {
		t.Errorf("expected SSECustomerKeyMD5 %q, got %q", keyMD5, info.SSECustomerKeyMD5)
	}

	// Attempt GET without SSE-C headers
	_, _, err = engine.GetObject(context.Background(), bucket, "secret.txt", "")
	if err == nil {
		t.Error("expected error getting SSE-C object without key")
	}

	// Attempt GET with wrong SSE-C key
	wrongKey := []byte("wrongwrongwrongwrongwrongwrongwr")
	wrongHash := md5.Sum(wrongKey)
	wrongKeyMD5 := base64.StdEncoding.EncodeToString(wrongHash[:])
	wrongParams := &SSECParams{
		Algorithm: "AES256",
		Key:       wrongKey,
		KeyMD5:    wrongKeyMD5,
	}
	wrongCtx := context.WithValue(context.Background(), SSECContextKey, wrongParams)
	_, _, err = engine.GetObject(wrongCtx, bucket, "secret.txt", "")
	if err == nil {
		t.Error("expected error getting SSE-C object with incorrect key")
	}

	// GET with correct key
	reader, getInfo, err := engine.GetObject(ctx, bucket, "secret.txt", "")
	if err != nil {
		t.Fatalf("GetObject with correct SSE-C key failed: %v", err)
	}
	defer reader.Close()

	retrieved, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read object content: %v", err)
	}

	if !bytes.Equal(retrieved, content) {
		t.Errorf("expected %q, got %q", content, retrieved)
	}
	if getInfo.SSECustomerAlgorithm != "AES256" {
		t.Errorf("expected SSECustomerAlgorithm AES256, got %q", getInfo.SSECustomerAlgorithm)
	}
}

func TestStorageCompression(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "compress-bucket"
	err := engine.CreateBucket(bucket)
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	// Compressible content: large text file
	content := []byte(strings.Repeat("this is a highly compressible text file containing repeating sentences. ", 100))
	info, err := engine.PutObject(context.Background(), bucket, "compressed.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	if !info.Compressed {
		t.Error("expected info.Compressed to be true for text/plain")
	}

	// Retrieve file
	reader, getInfo, err := engine.GetObject(context.Background(), bucket, "compressed.txt", "")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	defer reader.Close()

	if !getInfo.Compressed {
		t.Error("expected getInfo.Compressed to be true")
	}

	retrieved, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read decompressed content: %v", err)
	}

	if !bytes.Equal(retrieved, content) {
		t.Errorf("retrieved content length mismatch: got %d, expected %d", len(retrieved), len(content))
	}

	// Verify that the file size on disk is actually smaller than the original size (or at least compressed)
	path, err := engine.objectPath(bucket, "compressed.txt")
	if err != nil {
		t.Fatalf("failed to resolve path: %v", err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}
	if stat.Size() >= int64(len(content)) {
		t.Errorf("expected compressed file size on disk (%d) to be smaller than original (%d)", stat.Size(), len(content))
	}
}

func TestPrometheusMetrics(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "metrics-bucket"
	err := engine.CreateBucket(bucket)
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	// Reset metrics before test run to isolate counters
	atomic.StoreUint64(&GlobalMetrics.RequestsTotal, 0)
	atomic.StoreUint64(&GlobalMetrics.RequestErrors, 0)
	atomic.StoreUint64(&GlobalMetrics.BytesUploaded, 0)
	atomic.StoreUint64(&GlobalMetrics.BytesDownloaded, 0)
	atomic.StoreInt64(&GlobalMetrics.ActiveMultiparts, 0)

	// Upload object to trigger metrics
	content := []byte("hello metrics")
	_, err = engine.PutObject(context.Background(), bucket, "hello.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	if atomic.LoadUint64(&GlobalMetrics.BytesUploaded) != uint64(len(content)) {
		t.Errorf("expected BytesUploaded to be %d, got %d", len(content), atomic.LoadUint64(&GlobalMetrics.BytesUploaded))
	}

	// Download object to trigger metrics
	reader, _, err := engine.GetObject(context.Background(), bucket, "hello.txt", "")
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	_, err = io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("failed to read object: %v", err)
	}

	if atomic.LoadUint64(&GlobalMetrics.BytesDownloaded) != uint64(len(content)) {
		t.Errorf("expected BytesDownloaded to be %d, got %d", len(content), atomic.LoadUint64(&GlobalMetrics.BytesDownloaded))
	}

	// Verify formatting
	res := GlobalMetrics.FormatPrometheus(engine.DataDir())
	if !strings.Contains(res, "stiva_bytes_uploaded_total 13") {
		t.Errorf("expected prometheus payload to contain uploaded bytes 13, got:\n%s", res)
	}
	if !strings.Contains(res, "stiva_bytes_downloaded_total 13") {
		t.Errorf("expected prometheus payload to contain downloaded bytes 13, got:\n%s", res)
	}
	if !strings.Contains(res, "stiva_disk_total_bytes") {
		t.Errorf("expected prometheus payload to contain disk metrics, got:\n%s", res)
	}
}

func TestOutboundMirroring(t *testing.T) {
	// Setup a mock S3 receiver server
	var mu sync.Mutex
	var receivedPut, receivedDelete bool
	var receivedPath string
	var authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		authHeader = r.Header.Get("Authorization")
		receivedPath = r.URL.Path
		switch r.Method {
		case "PUT":
			receivedPut = true
			w.WriteHeader(http.StatusOK)
		case "DELETE":
			receivedDelete = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	// Configure environment variables for replication
	os.Setenv("STIVA_SYNC_ENDPOINT", server.URL)
	os.Setenv("STIVA_SYNC_BUCKET", "backup-bucket")
	os.Setenv("STIVA_SYNC_ACCESS_KEY", "accesskey")
	os.Setenv("STIVA_SYNC_SECRET_KEY", "secretkey")
	os.Setenv("STIVA_SYNC_REGION", "us-east-1")
	defer func() {
		os.Unsetenv("STIVA_SYNC_ENDPOINT")
		os.Unsetenv("STIVA_SYNC_BUCKET")
		os.Unsetenv("STIVA_SYNC_ACCESS_KEY")
		os.Unsetenv("STIVA_SYNC_SECRET_KEY")
		os.Unsetenv("STIVA_SYNC_REGION")
	}()

	tmpDir := t.TempDir()
	syncCfg := syncConfigFromEnv()
	engine, err := NewFilesystemEngine(tmpDir, syncCfg, "")
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	bucket := "sync-bucket"
	err = engine.CreateBucket(bucket)
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	// 1. Perform PutObject and check async mirroring
	content := []byte("sync data")
	_, err = engine.PutObject(context.Background(), bucket, "sync.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("failed to PutObject: %v", err)
	}

	// Wait up to 2 seconds for async sync execution
	for i := 0; i < 20; i++ {
		mu.Lock()
		put := receivedPut
		mu.Unlock()
		if put {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	put := receivedPut
	path := receivedPath
	auth := authHeader
	mu.Unlock()

	if !put {
		t.Error("expected mock server to receive PUT request for replication")
	}
	// The source bucket is part of the destination key, so source buckets
	// sharing a key name cannot overwrite each other on the mirror.
	if path != "/backup-bucket/"+bucket+"/sync.txt" {
		t.Errorf("expected path /backup-bucket/%s/sync.txt, got %q", bucket, path)
	}
	if !strings.Contains(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("expected Authorization header to contain AWS4-HMAC-SHA256, got %q", auth)
	}

	// Reset mock states
	mu.Lock()
	receivedPath = ""
	authHeader = ""
	mu.Unlock()

	// 2. Perform DeleteObject and check async mirroring
	_, _, err = engine.DeleteObject(bucket, "sync.txt", "")
	if err != nil {
		t.Fatalf("failed to DeleteObject: %v", err)
	}

	// Wait up to 2 seconds for async sync execution
	for i := 0; i < 20; i++ {
		mu.Lock()
		del := receivedDelete
		mu.Unlock()
		if del {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	del := receivedDelete
	pathDel := receivedPath
	mu.Unlock()

	if !del {
		t.Error("expected mock server to receive DELETE request for replication")
	}
	if pathDel != "/backup-bucket/"+bucket+"/sync.txt" {
		t.Errorf("expected path /backup-bucket/%s/sync.txt, got %q", bucket, pathDel)
	}
}

func TestPathTraversalProtectionExtended(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "traversal-bucket"
	engine.CreateBucket(bucket)

	// Test 1: Path Traversal via Version ID in GetObject/HeadObject/DeleteObject
	// The path traversal protection should detect and block these attempts.
	traversalVersions := []string{
		"../../etc/passwd",
		"..\\..\\Windows\\System32\\cmd.exe",
		"sub/../../passwd",
	}

	for _, badVersion := range traversalVersions {
		_, _, err := engine.GetObject(context.Background(), bucket, "test.txt", badVersion)
		if err == nil {
			t.Errorf("expected path traversal error for version ID %q, got nil", badVersion)
		}

		_, err = engine.HeadObject(context.Background(), bucket, "test.txt", badVersion)
		if err == nil {
			t.Errorf("expected path traversal error for version ID %q, got nil", badVersion)
		}

		_, _, err = engine.DeleteObject(bucket, "test.txt", badVersion)
		if err == nil {
			t.Errorf("expected path traversal error for version ID %q, got nil", badVersion)
		}
	}

	// Test 2: Path Traversal via Upload ID in multipart helpers
	traversalUploadIDs := []string{
		"../../etc/passwd",
		"..\\..\\Windows\\System32\\cmd.exe",
		"sub/../../passwd",
	}

	for _, badUploadID := range traversalUploadIDs {
		_, err := engine.UploadPart(context.Background(), bucket, "test.txt", badUploadID, 1, bytes.NewReader([]byte("data")), 4)
		if err == nil {
			t.Errorf("expected path traversal error for upload ID %q in UploadPart, got nil", badUploadID)
		}

		_, err = engine.CompleteMultipartUpload(context.Background(), bucket, "test.txt", badUploadID, []CompletePart{{PartNumber: 1, ETag: "some-etag"}})
		if err == nil {
			t.Errorf("expected path traversal error for upload ID %q in CompleteMultipartUpload, got nil", badUploadID)
		}

		err = engine.AbortMultipartUpload(bucket, "test.txt", badUploadID)
		if err == nil {
			t.Errorf("expected path traversal error for upload ID %q in AbortMultipartUpload, got nil", badUploadID)
		}
	}
}

func TestDoubleDotPrefixedKeys(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "dotdot-bucket"
	engine.CreateBucket(bucket)

	// Valid key starting with double dots, but not a path traversal
	validKey := "..hiddenfile"
	content := []byte("not-traversal")
	_, err := engine.PutObject(context.Background(), bucket, validKey, bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("failed to PutObject for valid key %q: %v", validKey, err)
	}

	reader, _, err := engine.GetObject(context.Background(), bucket, validKey, "")
	if err != nil {
		t.Fatalf("failed to GetObject for valid key %q: %v", validKey, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read data: %v", err)
	}
	if string(data) != "not-traversal" {
		t.Errorf("unexpected content: got %q, want 'not-traversal'", string(data))
	}
}

func TestDeleteBucketVersionedEmptiness(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "version-delete-bucket"

	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatalf("CreateBucket failed: %v", err)
	}

	if err := engine.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatalf("SetBucketVersioning failed: %v", err)
	}

	// Put version 1
	info, err := engine.PutObject(context.Background(), bucket, "file.txt", bytes.NewReader([]byte("v1")), 2, "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Delete it (creates a delete marker)
	if _, _, err := engine.DeleteObject(bucket, "file.txt", ""); err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// ListAllObjectMetas will return 1 object because the latest pointer is a delete marker
	objects, err := engine.Metadata().ListAllObjectMetas(bucket)
	if err != nil {
		t.Fatalf("ListAllObjectMetas failed: %v", err)
	}
	if len(objects) != 1 || !objects[0].IsDeleteMarker {
		t.Errorf("expected 1 delete marker, got %d", len(objects))
	}

	// DeleteBucket should fail because it has delete markers and historical version
	err = engine.DeleteBucket(bucket)
	if err == nil {
		t.Fatal("expected error when deleting bucket with version history / delete markers")
	}

	// Permanently delete version 1 and delete marker to empty it
	if _, _, err := engine.DeleteObject(bucket, "file.txt", info.VersionID); err != nil {
		t.Fatalf("DeleteObject version failed: %v", err)
	}

	// Get latest delete marker to delete it
	// Actually, let's delete the delete marker using its version ID
	// Let's find it in history
	var delMarkerID string
	if db, releasedb, err := engine.Metadata().acquireBucketDB(bucket); err == nil {
		db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("objects"))
			c := b.Cursor()
			prefix := []byte(bucket + "\x00")
			for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
				var info ObjectInfo
				if json.Unmarshal(v, &info) == nil && info.IsDeleteMarker {
					delMarkerID = info.VersionID
					break
				}
			}
			return nil
		})
		releasedb()
	}
	if delMarkerID != "" {
		engine.DeleteObject(bucket, "file.txt", delMarkerID)
	}

	// Now it should be empty and deletable
	if err := engine.DeleteBucket(bucket); err != nil {
		t.Fatalf("DeleteBucket failed after cleaning history: %v", err)
	}
}

func TestFlatNamespacePathConflicts(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "conflict-bucket"
	engine.CreateBucket(bucket)

	// Put object 'a'
	_, err := engine.PutObject(context.Background(), bucket, "a", bytes.NewReader([]byte("file-a")), 6, "text/plain")
	if err != nil {
		t.Fatalf("failed to PutObject 'a': %v", err)
	}

	// Attempting to put object 'a/b' should conflict (directory-file conflict)
	_, err = engine.PutObject(context.Background(), bucket, "a/b", bytes.NewReader([]byte("file-b")), 6, "text/plain")
	if err == nil {
		t.Fatal("expected conflict error when uploading 'a/b' because 'a' is a file")
	}
	var s3Err *S3Error
	ok := errors.As(err, &s3Err)
	if !ok || s3Err.Code != "InvalidRequest" {
		t.Errorf("expected S3Error with code 'InvalidRequest', got %v", err)
	}

	// Clean up 'a'
	engine.DeleteObject(bucket, "a", "")

	// Put object 'a/b' (now should succeed)
	_, err = engine.PutObject(context.Background(), bucket, "a/b", bytes.NewReader([]byte("file-b")), 6, "text/plain")
	if err != nil {
		t.Fatalf("failed to PutObject 'a/b': %v", err)
	}

	// Attempting to put object 'a' should conflict (file-directory conflict)
	_, err = engine.PutObject(context.Background(), bucket, "a", bytes.NewReader([]byte("file-a")), 6, "text/plain")
	if err == nil {
		t.Fatal("expected conflict error when uploading 'a' because 'a/b' exists as a sub-path")
	}
	ok = errors.As(err, &s3Err)
	if !ok || s3Err.Code != "InvalidRequest" {
		t.Errorf("expected S3Error with code 'InvalidRequest', got %v", err)
	}
}

func TestMultipartConstraintsValidation(t *testing.T) {
	engine := setupTestEngine(t)
	// Re-enable the 5MB minimum so this test can exercise enforcement.
	engine.SetDisableMinPartSize(false)

	bucket := "multipart-validation-bucket"
	engine.CreateBucket(bucket)

	upload, err := engine.CreateMultipartUpload(bucket, "large.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	// Part 1: upload 1 MB (too small for intermediate part)
	p1Data := bytes.Repeat([]byte("A"), 1*1024*1024)
	p1, err := engine.UploadPart(context.Background(), bucket, "large.bin", upload.UploadID, 1, bytes.NewReader(p1Data), int64(len(p1Data)))
	if err != nil {
		t.Fatalf("UploadPart 1 failed: %v", err)
	}

	// Part 2: upload 1 MB (this will be the last part, which can be small)
	p2Data := bytes.Repeat([]byte("B"), 1*1024*1024)
	p2, err := engine.UploadPart(context.Background(), bucket, "large.bin", upload.UploadID, 2, bytes.NewReader(p2Data), int64(len(p2Data)))
	if err != nil {
		t.Fatalf("UploadPart 2 failed: %v", err)
	}

	parts := []CompletePart{
		{PartNumber: 1, ETag: p1.ETag},
		{PartNumber: 2, ETag: p2.ETag},
	}

	// Completing should fail because Part 1 is < 5MB and is not the last part
	_, err = engine.CompleteMultipartUpload(context.Background(), bucket, "large.bin", upload.UploadID, parts)
	if err == nil {
		t.Fatal("expected EntityTooSmall error, got nil")
	}
	var s3Err *S3Error
	ok := errors.As(err, &s3Err)
	if !ok || s3Err.Code != "EntityTooSmall" {
		t.Errorf("expected EntityTooSmall, got %v", err)
	}

	// Clean up / abort
	engine.AbortMultipartUpload(bucket, "large.bin", upload.UploadID)
}

func TestBucketLifecycleAndLogging(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "lifecycle-bucket"
	err := engine.CreateBucket(bucket)
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	// 1. Test Put/Get/Delete Lifecycle
	lc := &LifecycleConfiguration{
		Rules: []LifecycleRule{
			{
				ID:         "expire-logs",
				Status:     "Enabled",
				Filter:     LifecycleFilter{Prefix: "logs/"},
				Expiration: &LifecycleExpiration{Days: 1},
			},
			{
				ID:                          "expire-archive-noncurrent",
				Status:                      "Enabled",
				Filter:                      LifecycleFilter{Prefix: "archive/"},
				NoncurrentVersionExpiration: &NoncurrentVersionExpiration{NoncurrentDays: 1},
			},
		},
	}

	err = engine.PutBucketLifecycle(bucket, lc)
	if err != nil {
		t.Fatalf("PutBucketLifecycle failed: %v", err)
	}

	fetchedLc, err := engine.GetBucketLifecycle(bucket)
	if err != nil {
		t.Fatalf("GetBucketLifecycle failed: %v", err)
	}
	if len(fetchedLc.Rules) != 2 || fetchedLc.Rules[0].ID != "expire-logs" {
		t.Errorf("fetched rules mismatch: %+v", fetchedLc.Rules)
	}

	err = engine.DeleteBucketLifecycle(bucket)
	if err != nil {
		t.Fatalf("DeleteBucketLifecycle failed: %v", err)
	}

	_, err = engine.GetBucketLifecycle(bucket)
	if err == nil {
		t.Error("expected error getting deleted lifecycle, got nil")
	}

	// 2. Test Expiration Execution
	expBucket := "expiration-bucket"
	engine.CreateBucket(expBucket)
	engine.PutBucketLifecycle(expBucket, lc)
	engine.SetBucketVersioning(expBucket, "Enabled")

	ctx := context.Background()

	// Put log file that will be expired
	logInfo, err := engine.PutObject(ctx, expBucket, "logs/test.log", strings.NewReader("log-data"), 8, "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}
	// Modify to be 48 hours ago
	logInfo.LastModified = time.Now().UTC().Add(-48 * time.Hour)
	err = engine.Metadata().PutObjectMetaRaw(logInfo)
	if err != nil {
		t.Fatalf("PutObjectMetaRaw failed: %v", err)
	}

	// Put log file that will NOT be expired
	_, err = engine.PutObject(ctx, expBucket, "logs/keep.log", strings.NewReader("keep-data"), 9, "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Put version 1 (will be expired noncurrent version)
	v1Info, err := engine.PutObject(ctx, expBucket, "archive/versioned.txt", strings.NewReader("v1"), 2, "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Put version 2 (latest current version, will NOT be expired)
	v2Info, err := engine.PutObject(ctx, expBucket, "archive/versioned.txt", strings.NewReader("v2-latest"), 9, "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Modify v1 to be 48 hours ago and noncurrent
	v1Info.LastModified = time.Now().UTC().Add(-48 * time.Hour)
	v1Info.IsLatest = false
	err = engine.Metadata().PutObjectMetaRaw(v1Info)
	if err != nil {
		t.Fatalf("PutObjectMetaRaw failed: %v", err)
	}

	// Execute CleanExpiredObjects
	err = engine.CleanExpiredObjects()
	if err != nil {
		t.Fatalf("CleanExpiredObjects failed: %v", err)
	}

	// Verify logs/test.log current version was expired (since versioning is Enabled, it should create a delete marker)
	rTest, _, err := engine.GetObject(ctx, expBucket, "logs/test.log", "")
	if err == nil {
		rTest.Close()
		t.Error("expected logs/test.log to be expired and unavailable, but got nil error")
	} else {
		var s3Err *S3Error
		ok := errors.As(err, &s3Err)
		if !ok || s3Err.Code != "NoSuchKey" {
			t.Errorf("expected NoSuchKey error, got %v", err)
		}
	}

	// Verify logs/keep.log still exists
	rKeep, _, err := engine.GetObject(ctx, expBucket, "logs/keep.log", "")
	if err != nil {
		t.Errorf("expected logs/keep.log to exist, got %v", err)
	} else {
		rKeep.Close()
	}

	// Verify archive/versioned.txt v1 is deleted (NoSuchKey or similar for that specific version ID)
	rV1, _, err := engine.GetObject(ctx, expBucket, "archive/versioned.txt", v1Info.VersionID)
	if err == nil {
		rV1.Close()
		t.Error("expected versioned v1 to be permanently deleted, got nil error")
	}

	// Verify archive/versioned.txt v2 still exists
	rV2, _, err := engine.GetObject(ctx, expBucket, "archive/versioned.txt", v2Info.VersionID)
	if err != nil {
		t.Errorf("expected latest version to still exist, got %v", err)
	} else {
		rV2.Close()
	}

	// 3. Test Logging
	loggingStatus := &BucketLoggingStatus{
		LoggingEnabled: &LoggingEnabled{
			TargetBucket: bucket,
			TargetPrefix: "s3-access-logs/",
		},
	}
	err = engine.PutBucketLogging(expBucket, loggingStatus)
	if err != nil {
		t.Fatalf("PutBucketLogging failed: %v", err)
	}

	fetchedLogging, err := engine.GetBucketLogging(expBucket)
	if err != nil {
		t.Fatalf("GetBucketLogging failed: %v", err)
	}
	if fetchedLogging == nil || fetchedLogging.LoggingEnabled == nil || fetchedLogging.LoggingEnabled.TargetBucket != bucket {
		t.Errorf("fetched logging mismatch: %+v", fetchedLogging)
	}
}

func TestConcurrentWriteDelete(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "concurrent-bucket"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	const numWorkers = 20
	const numOpsPerWorker = 50
	var wg sync.WaitGroup

	ctx := context.Background()

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < numOpsPerWorker; i++ {
				key := fmt.Sprintf("worker-%d/key-%d", workerID, i)
				content := fmt.Sprintf("content-%d", i)

				_, err := engine.PutObject(ctx, bucket, key, strings.NewReader(content), int64(len(content)), "text/plain")
				if err != nil {
					t.Errorf("PutObject failed during concurrent test: %v", err)
					return
				}

				if i%2 == 0 {
					r, _, err := engine.GetObject(ctx, bucket, key, "")
					if err == nil {
						r.Close()
					}
				}

				_, _, err = engine.DeleteObject(bucket, key, "")
				if err != nil {
					t.Errorf("DeleteObject failed during concurrent test: %v", err)
					return
				}
			}
		}(w)
	}

	wg.Wait()

	isEmpty, err := engine.Metadata().IsBucketEmpty(bucket)
	if err != nil {
		t.Fatalf("IsBucketEmpty failed: %v", err)
	}
	if !isEmpty {
		t.Errorf("expected bucket to be empty, but it is not")
	}
}

func TestPathTraversalInvalidChars(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "safety-bucket"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	ctx := context.Background()

	// Test 1: Keys containing null byte
	nullByteKeys := []string{
		"test\x00key",
		"\x00hello",
		"folder/\x00file",
	}
	for _, badKey := range nullByteKeys {
		_, err := engine.PutObject(ctx, bucket, badKey, strings.NewReader("data"), 4, "text/plain")
		if err == nil {
			t.Errorf("expected error or rejection for key with null byte %q, got nil", badKey)
		}
	}

	// Test 2: Keys containing control characters (\r, \n, \t)
	controlCharKeys := []string{
		"test\rkey",
		"test\nkey",
		"test\tkey",
		"folder\n/file",
	}
	for _, badKey := range controlCharKeys {
		_, err := engine.PutObject(ctx, bucket, badKey, strings.NewReader("data"), 4, "text/plain")
		if err == nil {
			r, _, errGet := engine.GetObject(ctx, bucket, badKey, "")
			if errGet != nil {
				t.Errorf("failed to read back key with control char %q: %v", badKey, errGet)
			} else {
				r.Close()
			}
			_, _, errDel := engine.DeleteObject(bucket, badKey, "")
			if errDel != nil {
				t.Errorf("failed to delete key with control char %q: %v", badKey, errDel)
			}
		}
	}

	// Test 3: Path Traversal with URL-encoded sequences
	urlEncodedKeys := []string{
		"%2e%2e%2f%2e%2e%2fetc/passwd",
		"..%2f..%2fetc/passwd",
		"sub/%2e%2e/passwd",
	}
	for _, badKey := range urlEncodedKeys {
		_, err := engine.PutObject(ctx, bucket, badKey, strings.NewReader("data"), 4, "text/plain")
		if err == nil {
			resolvedPath, errPath := engine.objectPath(bucket, badKey)
			if errPath != nil {
				t.Errorf("failed to get object path for %q: %v", badKey, errPath)
			}
			expectedPrefix := engine.bucketPath(bucket)
			if !strings.HasPrefix(resolvedPath, expectedPrefix) {
				t.Errorf("path %q escaped bucket directory %q", resolvedPath, expectedPrefix)
			}
			engine.DeleteObject(bucket, badKey, "")
		}
	}
}

func TestGetObjectRangeSeek(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("seek-bucket")

	content := []byte("abcdefghijklmnopqrstuvwxyz")
	ctx := context.Background()
	_, err := engine.PutObject(ctx, "seek-bucket", "alphabet.txt", bytes.NewReader(content), int64(len(content)), "application/octet-stream")
	if err != nil {
		t.Fatalf("failed to put object: %v", err)
	}

	reader, _, err := engine.GetObject(ctx, "seek-bucket", "alphabet.txt", "")
	if err != nil {
		t.Fatalf("failed to get object: %v", err)
	}
	defer reader.Close()

	seeker, ok := reader.(io.ReadSeeker)
	if !ok {
		t.Fatal("expected reader to implement io.ReadSeeker")
	}

	// Seek to 10
	pos, err := seeker.Seek(10, io.SeekStart)
	if err != nil {
		t.Fatalf("failed to seek: %v", err)
	}
	if pos != 10 {
		t.Errorf("expected position 10, got %d", pos)
	}

	// Read 5 bytes
	buf := make([]byte, 5)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if n != 5 {
		t.Errorf("expected to read 5 bytes, got %d", n)
	}
	if string(buf) != "klmno" {
		t.Errorf("expected 'klmno', got %q", string(buf))
	}

	// Seek forward 2 bytes from current
	pos, err = seeker.Seek(2, io.SeekCurrent)
	if err != nil {
		t.Fatalf("failed to seek: %v", err)
	}
	if pos != 17 { // 10 + 5 + 2
		t.Errorf("expected position 17, got %d", pos)
	}

	// Read 3 bytes
	buf = make([]byte, 3)
	n, err = reader.Read(buf)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 bytes read, got %d", n)
	}
	if string(buf) != "rst" {
		t.Errorf("expected 'rst', got %q", string(buf))
	}
}

func TestGracefulShutdown(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("shutdown-bucket")

	// Set webhook URL and sync endpoint env variables to force initialization of dispatchers
	t.Setenv("STIVA_WEBHOOK_URL", "http://localhost:12345/webhook")
	t.Setenv("STIVA_SYNC_ENDPOINT", "http://localhost:12345")
	t.Setenv("STIVA_SYNC_BUCKET", "backup-bucket")

	ctx := context.Background()
	info, err := engine.PutObject(ctx, "shutdown-bucket", "test.txt", strings.NewReader("content"), 7, "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Triggering activities will initialize the dispatchers and worker goroutines
	engine.triggerWebhook("Put", info)
	engine.MirrorSync("shutdown-bucket", "test.txt", "PUT")

	// Let's call Close which will stop the dispatchers
	err = engine.Close()
	if err != nil {
		t.Fatalf("engine.Close failed: %v", err)
	}

	// Verify that the shutdown flags are set
	if !engine.isSyncShuttingDownNow() {
		t.Error("expected isSyncShuttingDown to be true")
	}
	if !engine.isWebhookShuttingDownNow() {
		t.Error("expected isWebhookShuttingDown to be true")
	}

	// Try triggering again - it should be ignored or warning should be printed without panic/block
	engine.triggerWebhook("Put", info)
	engine.MirrorSync("shutdown-bucket", "test.txt", "PUT")
}

// TestCompressionIsSizeAware covers the rule that keeps large objects
// seekable. A compressed object's reader is a gzip stream, so every Range
// request against one has to decompress from byte zero to reach the offset —
// O(n) per request, on exactly the large text and log files where ranged reads
// matter most.
func TestCompressionIsSizeAware(t *testing.T) {
	engine := setupTestEngine(t)
	bucket := "compress-policy"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	small := strings.Repeat("a", 1024)
	large := strings.Repeat("a", maxCompressibleSize+1)

	tests := []struct {
		name           string
		key            string
		body           string
		size           int64
		contentType    string
		wantCompressed bool
	}{
		{"small text is compressed", "small.txt", small, int64(len(small)), "text/plain", true},
		{"large text is not", "large.txt", large, int64(len(large)), "text/plain", false},
		{"unknown size is not", "stream.txt", small, -1, "text/plain", false},
		{"binary is never compressed", "blob.bin", small, int64(len(small)), "application/octet-stream", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, err := engine.PutObject(context.Background(), bucket, tc.key,
				strings.NewReader(tc.body), tc.size, tc.contentType)
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			if info.Compressed != tc.wantCompressed {
				t.Errorf("Compressed = %v, want %v", info.Compressed, tc.wantCompressed)
			}

			// Whatever the policy chose, the bytes must round-trip.
			rc, _, err := engine.GetObject(context.Background(), bucket, tc.key, "")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.body {
				t.Errorf("round-trip mismatch: got %d bytes, want %d", len(got), len(tc.body))
			}

			// An uncompressed object must be seekable, which is what lets
			// GetObject range requests avoid decompressing from the start.
			_, isSeekable := rc.(io.ReadSeeker)
			if isSeekable != !tc.wantCompressed {
				t.Errorf("seekable = %v, want %v", isSeekable, !tc.wantCompressed)
			}
		})
	}
}
