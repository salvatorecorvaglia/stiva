package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestPutObjectEnforcesMaxObjectSize guards the operator-configurable object
// size ceiling. Before this, the only size limit was enforced by the SigV4
// auth layer (auth.MaxPayloadSize), which a client sending
// X-Amz-Content-Sha256: UNSIGNED-PAYLOAD bypasses entirely — the storage
// engine itself had no cap of its own. This checks both a declared
// Content-Length over the cap (rejected up front) and an unknown/chunked
// size (-1, rejected only once the actual bytes exceed the cap).
func TestPutObjectEnforcesMaxObjectSize(t *testing.T) {
	engine := setupTestEngine(t)
	engine.SetMaxObjectSize(10)
	engine.CreateBucket("capped")

	t.Run("declared size over cap is rejected up front", func(t *testing.T) {
		content := bytes.Repeat([]byte("a"), 20)
		_, err := engine.PutObject(context.Background(), "capped", "big.txt",
			bytes.NewReader(content), int64(len(content)), "text/plain")
		assertEntityTooLarge(t, err)
	})

	t.Run("unknown size (chunked) over cap is rejected once exceeded", func(t *testing.T) {
		content := bytes.Repeat([]byte("b"), 20)
		_, err := engine.PutObject(context.Background(), "capped", "chunked.txt",
			bytes.NewReader(content), -1, "text/plain")
		assertEntityTooLarge(t, err)
	})

	t.Run("content at or under the cap is accepted", func(t *testing.T) {
		content := bytes.Repeat([]byte("c"), 10)
		info, err := engine.PutObject(context.Background(), "capped", "ok.txt",
			bytes.NewReader(content), int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("expected content at the cap to be accepted, got: %v", err)
		}
		if info.Size != 10 {
			t.Errorf("size = %d, want 10", info.Size)
		}
	})
}

// TestUploadPartEnforcesMaxObjectSize mirrors TestPutObjectEnforcesMaxObjectSize
// for multipart parts.
func TestUploadPartEnforcesMaxObjectSize(t *testing.T) {
	engine := setupTestEngine(t)
	engine.SetMaxObjectSize(10)
	engine.CreateBucket("capped-mp")

	info, err := engine.CreateMultipartUpload("capped-mp", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	content := bytes.Repeat([]byte("a"), 20)
	_, err = engine.UploadPart(context.Background(), "capped-mp", "big.bin", info.UploadID, 1,
		bytes.NewReader(content), int64(len(content)))
	assertEntityTooLarge(t, err)
}

// TestMaxObjectSizeZeroDisablesCap confirms the default (unset) behavior:
// zero must mean "no cap", not "reject everything".
func TestMaxObjectSizeZeroDisablesCap(t *testing.T) {
	engine := setupTestEngine(t)
	engine.CreateBucket("uncapped")

	content := bytes.Repeat([]byte("z"), 1<<20) // 1MB, well within any real limit
	_, err := engine.PutObject(context.Background(), "uncapped", "file.bin",
		bytes.NewReader(content), int64(len(content)), "application/octet-stream")
	if err != nil {
		t.Fatalf("expected no cap to allow this upload, got: %v", err)
	}
}

func assertEntityTooLarge(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an EntityTooLarge error, got nil")
	}
	var s3Err *S3Error
	if !errors.As(err, &s3Err) {
		t.Fatalf("expected *S3Error, got %T: %v", err, err)
	}
	if s3Err.Code != "EntityTooLarge" {
		t.Errorf("error code = %q, want EntityTooLarge", s3Err.Code)
	}
}
