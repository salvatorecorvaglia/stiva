package storage_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func ssecParamsForKey(key []byte) *storage.SSECParams {
	sum := md5.Sum(key)
	return &storage.SSECParams{
		Algorithm: "AES256",
		Key:       key,
		KeyMD5:    base64.StdEncoding.EncodeToString(sum[:]),
	}
}

// TestMultipartSSECKeyIsNeverPersisted is the regression test for the audit
// finding that MultipartMeta serialised the customer-provided SSE-C key into
// bbolt in plaintext. That defeated the entire purpose of SSE-C: read access to
// the metadata database was enough to decrypt every multipart object.
func TestMultipartSSECKeyIsNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	fs, err := storage.NewFilesystemEngine(dir, nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer fs.Close()

	if err := fs.CreateBucket("secure"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// A distinctive key so we can search the raw database bytes for it.
	customerKey := bytes.Repeat([]byte{0xAB}, 32)
	params := ssecParamsForKey(customerKey)
	ctx := context.WithValue(context.Background(), storage.SSECContextKey, params)

	up, err := fs.CreateMultipartUpload("secure", "secret.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("create mpu: %v", err)
	}

	body := strings.Repeat("s", 32)
	if _, err := fs.UploadPart(ctx, "secure", "secret.bin", up.UploadID, 1,
		strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("upload part: %v", err)
	}

	// Force the metadata to disk before inspecting it.
	if err := fs.Metadata().Close(); err != nil {
		t.Fatalf("close metadata: %v", err)
	}

	dbPath := filepath.Join(dir, "metadata", "secure.db")
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read metadata db: %v", err)
	}

	if bytes.Contains(raw, customerKey) {
		t.Fatal("SSE-C customer key found verbatim in the metadata database")
	}
	// The JSON field itself must be gone, not merely empty.
	if bytes.Contains(raw, []byte("sseCustomerKey\"")) {
		t.Error("metadata database still carries an sseCustomerKey field")
	}
	// The key MD5 is legitimately retained for validation.
	if !bytes.Contains(raw, []byte("sseCustomerKeyMD5")) {
		t.Error("expected the key MD5 to still be recorded for validation")
	}
}

// TestMultipartSSECRoundTrip verifies an SSE-C multipart upload still completes
// and reads back correctly now that the key must be re-presented on completion.
func TestMultipartSSECRoundTrip(t *testing.T) {
	fs, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer fs.Close()
	if err := fs.CreateBucket("secure"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	params := ssecParamsForKey(bytes.Repeat([]byte{0x11}, 32))
	ctx := context.WithValue(context.Background(), storage.SSECContextKey, params)

	up, err := fs.CreateMultipartUpload("secure", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("create mpu: %v", err)
	}

	// Two parts; the first must clear the 5MB minimum.
	part1 := strings.Repeat("A", 5*1024*1024)
	part2 := strings.Repeat("B", 1024)

	p1, err := fs.UploadPart(ctx, "secure", "big.bin", up.UploadID, 1,
		strings.NewReader(part1), int64(len(part1)))
	if err != nil {
		t.Fatalf("upload part 1: %v", err)
	}
	p2, err := fs.UploadPart(ctx, "secure", "big.bin", up.UploadID, 2,
		strings.NewReader(part2), int64(len(part2)))
	if err != nil {
		t.Fatalf("upload part 2: %v", err)
	}

	parts := []storage.CompletePart{
		{PartNumber: 1, ETag: p1.ETag},
		{PartNumber: 2, ETag: p2.ETag},
	}

	if _, err := fs.CompleteMultipartUpload(ctx, "secure", "big.bin", up.UploadID, parts); err != nil {
		t.Fatalf("complete: %v", err)
	}

	reader, info, err := fs.GetObject(ctx, "secure", "big.bin", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer reader.Close()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := part1 + part2
	if string(got) != want {
		t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(want))
	}
	if info.SSECustomerAlgorithm != "AES256" {
		t.Errorf("algorithm = %q, want AES256", info.SSECustomerAlgorithm)
	}
}

// TestMultipartSSECCompleteRequiresKey verifies completion is refused when the
// customer key is not re-presented, and when the wrong key is presented.
func TestMultipartSSECCompleteRequiresKey(t *testing.T) {
	fs, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer fs.Close()
	if err := fs.CreateBucket("secure"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	params := ssecParamsForKey(bytes.Repeat([]byte{0x22}, 32))
	ctx := context.WithValue(context.Background(), storage.SSECContextKey, params)

	up, err := fs.CreateMultipartUpload("secure", "k.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("create mpu: %v", err)
	}
	body := strings.Repeat("q", 64)
	p1, err := fs.UploadPart(ctx, "secure", "k.bin", up.UploadID, 1,
		strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("upload part: %v", err)
	}
	parts := []storage.CompletePart{{PartNumber: 1, ETag: p1.ETag}}

	// No key at all.
	_, err = fs.CompleteMultipartUpload(context.Background(), "secure", "k.bin", up.UploadID, parts)
	if err == nil {
		t.Fatal("completing an SSE-C upload without the customer key must fail")
	}

	// Wrong key.
	wrong := context.WithValue(context.Background(), storage.SSECContextKey,
		ssecParamsForKey(bytes.Repeat([]byte{0x33}, 32)))
	_, err = fs.CompleteMultipartUpload(wrong, "secure", "k.bin", up.UploadID, parts)
	if err == nil {
		t.Fatal("completing an SSE-C upload with the wrong customer key must fail")
	}
}

// TestCompleteMultipartRejectsDuplicateParts covers the audit finding that
// duplicate part numbers were accepted and concatenated twice.
func TestCompleteMultipartRejectsDuplicateParts(t *testing.T) {
	fs, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	fs.SetDisableMinPartSize(true)
	defer fs.Close()
	if err := fs.CreateBucket("dupes"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	up, err := fs.CreateMultipartUpload("dupes", "d.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("create mpu: %v", err)
	}
	body := strings.Repeat("d", 32)
	p1, err := fs.UploadPart(context.Background(), "dupes", "d.bin", up.UploadID, 1,
		strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("upload part: %v", err)
	}

	parts := []storage.CompletePart{
		{PartNumber: 1, ETag: p1.ETag},
		{PartNumber: 1, ETag: p1.ETag},
	}
	_, err = fs.CompleteMultipartUpload(context.Background(), "dupes", "d.bin", up.UploadID, parts)
	if err == nil {
		t.Fatal("duplicate part numbers must be rejected")
	}

	var s3Err *storage.S3Error
	if !errors.As(err, &s3Err) || s3Err.Code != "InvalidPartOrder" {
		t.Errorf("expected InvalidPartOrder, got %v", err)
	}
}
