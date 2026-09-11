package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentPutObjectSameKeyKeepsContentAndETagConsistent guards against a
// race where PutObject's rename-to-final-path and metadata write were two
// independent, unlocked steps. Two racing PUTs to the same unversioned key
// could interleave such that the winning rename's bytes on disk didn't match
// the winning metadata write's ETag, silently corrupting the object's
// integrity guarantee.
func TestConcurrentPutObjectSameKeyKeepsContentAndETagConsistent(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("race-bucket")

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := bytes.Repeat([]byte(fmt.Sprintf("%02d", i)), 1000+i)
			_, err := engine.PutObject(context.Background(), "race-bucket", "contested-key",
				bytes.NewReader(content), int64(len(content)), "application/octet-stream")
			if err != nil {
				t.Errorf("PutObject goroutine %d failed: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	reader, info, err := engine.GetObject(context.Background(), "race-bucket", "contested-key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}

	sum := md5.Sum(data)
	actualETag := hex.EncodeToString(sum[:])
	if actualETag != info.ETag {
		t.Errorf("metadata ETag %q does not match actual content MD5 %q — the winning rename and the winning metadata write came from different concurrent requests", info.ETag, actualETag)
	}
	if int64(len(data)) != info.Size {
		t.Errorf("metadata Size %d does not match actual content length %d", info.Size, len(data))
	}
}

// TestConcurrentUploadPartSamePartNumberKeepsContentAndETagConsistent mirrors
// the PutObject race above for UploadPart: the part file rename and the
// part's ETag/size metadata update were two independent, unlocked steps
// outside a brief metadata-only lock, so two racing UploadPart calls for the
// same part number could leave the on-disk part file from one request paired
// with the recorded ETag from another.
func TestConcurrentUploadPartSamePartNumberKeepsContentAndETagConsistent(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("race-bucket-mp")

	info, err := engine.CreateMultipartUpload("race-bucket-mp", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := bytes.Repeat([]byte(fmt.Sprintf("%02d", i)), 1000+i)
			_, err := engine.UploadPart(context.Background(), "race-bucket-mp", "big.bin", info.UploadID, 1,
				bytes.NewReader(content), int64(len(content)))
			if err != nil {
				t.Errorf("UploadPart goroutine %d failed: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	parts, err := engine.ListParts("race-bucket-mp", "big.bin", info.UploadID)
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected exactly 1 recorded part, got %d", len(parts))
	}
	recordedETag := parts[0].ETag
	recordedSize := parts[0].Size

	completed, err := engine.CompleteMultipartUpload(context.Background(), "race-bucket-mp", "big.bin", info.UploadID,
		[]CompletePart{{PartNumber: 1, ETag: recordedETag}})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	reader, _, err := engine.GetObject(context.Background(), "race-bucket-mp", "big.bin", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}

	sum := md5.Sum(data)
	actualPartETag := hex.EncodeToString(sum[:])
	if actualPartETag != strings.Trim(recordedETag, `"`) {
		t.Errorf("recorded part ETag %q does not match the actual on-disk part content MD5 %q — the winning rename and the winning metadata write came from different concurrent UploadPart requests", recordedETag, actualPartETag)
	}
	if int64(len(data)) != recordedSize {
		t.Errorf("recorded part size %d does not match actual content length %d", recordedSize, len(data))
	}
	if completed.Size != recordedSize {
		t.Errorf("completed object size %d does not match recorded part size %d", completed.Size, recordedSize)
	}
}
