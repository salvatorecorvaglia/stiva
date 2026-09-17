package storage

import (
	"os"
	"testing"
)

func TestBucketCORSSupport(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "stiva-cors-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	engine, err := NewFilesystemEngine(tempDir, nil, "")
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	bucketName := "test-cors-bucket"
	if err := engine.CreateBucket(bucketName); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	// Verify initially no CORS config
	cors, err := engine.GetBucketCORS(bucketName)
	if err != nil {
		t.Fatalf("failed to get initial CORS: %v", err)
	}
	if cors != nil {
		t.Errorf("expected nil CORS initially, got %+v", cors)
	}

	// Put CORS configuration
	newCORS := &CORSConfiguration{
		CORSRules: []CORSRule{
			{
				AllowedOrigins: []string{"https://example.com"},
				AllowedMethods: []string{"GET", "PUT", "POST"},
				AllowedHeaders: []string{"*"},
				MaxAgeSeconds:  3600,
			},
		},
	}

	if err := engine.PutBucketCORS(bucketName, newCORS); err != nil {
		t.Fatalf("failed to put CORS config: %v", err)
	}

	// Get CORS configuration
	fetchedCORS, err := engine.GetBucketCORS(bucketName)
	if err != nil {
		t.Fatalf("failed to get CORS config: %v", err)
	}
	if fetchedCORS == nil || len(fetchedCORS.CORSRules) != 1 {
		t.Fatalf("expected 1 CORS rule, got %+v", fetchedCORS)
	}
	if fetchedCORS.CORSRules[0].AllowedOrigins[0] != "https://example.com" {
		t.Errorf("expected origin 'https://example.com', got '%s'", fetchedCORS.CORSRules[0].AllowedOrigins[0])
	}

	// Delete CORS configuration
	if err := engine.DeleteBucketCORS(bucketName); err != nil {
		t.Fatalf("failed to delete CORS config: %v", err)
	}

	deletedCORS, err := engine.GetBucketCORS(bucketName)
	if err != nil {
		t.Fatalf("failed to get CORS config after delete: %v", err)
	}
	if deletedCORS != nil {
		t.Errorf("expected nil CORS after delete, got %+v", deletedCORS)
	}
}
