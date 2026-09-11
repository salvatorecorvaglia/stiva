package storage

import (
	"context"
	"strings"
	"testing"
)

func TestPathTraversalPrevention(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("safe-bucket"); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	traversalKeys := []string{
		"../secret.txt",
		"../../etc/passwd",
		"foo/../../bar",
		"..\\windows\\system32",
		"foo/..\\..\\bar",
	}

	for _, key := range traversalKeys {
		t.Run("objectPath_"+key, func(t *testing.T) {
			_, err := engine.objectPath("safe-bucket", key)
			if err == nil {
				t.Errorf("expected error for traversal key %q, got nil", key)
			}
		})

		t.Run("objectPathWithVersion_"+key, func(t *testing.T) {
			_, err := engine.objectPathWithVersion("safe-bucket", key, "version-1")
			if err == nil {
				t.Errorf("expected error for traversal key %q, got nil", key)
			}
		})

		t.Run("PutObject_"+key, func(t *testing.T) {
			r := strings.NewReader("data")
			_, err := engine.PutObject(context.Background(), "safe-bucket", key, r, 4, "text/plain")
			if err == nil {
				t.Errorf("expected error for PutObject traversal key %q, got nil", key)
			}
		})

		t.Run("GetObject_"+key, func(t *testing.T) {
			_, _, err := engine.GetObject(context.Background(), "safe-bucket", key, "")
			if err == nil {
				t.Errorf("expected error for GetObject traversal key %q, got nil", key)
			}
		})
	}
}

func TestPathTraversalVersionAndUploadID(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("safe-bucket"); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	traversalVersionIDs := []string{
		"../version",
		"ver/../sion",
		"..\\version",
		"/version",
	}

	for _, vID := range traversalVersionIDs {
		t.Run("versionID_"+vID, func(t *testing.T) {
			_, err := engine.objectPathWithVersion("safe-bucket", "valid-key.txt", vID)
			if err == nil {
				t.Errorf("expected error for traversal versionID %q, got nil", vID)
			}
		})
	}

	traversalUploadIDs := []string{
		"../upload",
		"up/../load",
		"..\\upload",
		"/upload",
	}

	for _, uID := range traversalUploadIDs {
		t.Run("uploadID_"+uID, func(t *testing.T) {
			_, err := engine.multipartUploadPath("safe-bucket", uID)
			if err == nil {
				t.Errorf("expected error for traversal uploadID %q, got nil", uID)
			}

			_, err = engine.multipartDir("safe-bucket", "valid-key.txt", uID)
			if err == nil {
				t.Errorf("expected error for traversal multipartDir uploadID %q, got nil", uID)
			}
		})
	}
}

func TestValidNestedKeys(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("safe-bucket"); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	validKeys := []string{
		"simple.txt",
		"photos/2024/vacation/img1.jpg",
		"a/b/c/d/e/f/file.dat",
		"dots.in.filename.tar.gz",
	}

	for _, key := range validKeys {
		t.Run("validKey_"+key, func(t *testing.T) {
			p, err := engine.objectPath("safe-bucket", key)
			if err != nil {
				t.Errorf("unexpected error for valid key %q: %v", key, err)
			}
			if !strings.Contains(p, "safe-bucket") {
				t.Errorf("expected path to contain bucket name, got %s", p)
			}
		})
	}
}
