package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func putString(t *testing.T, engine *FilesystemEngine, bucket, key, body string) *ObjectInfo {
	t.Helper()
	info, err := engine.PutObject(context.Background(), bucket, key, strings.NewReader(body), int64(len(body)), "application/octet-stream")
	if err != nil {
		t.Fatalf("PutObject %s=%q: %v", key, body, err)
	}
	return info
}

func readVersion(t *testing.T, engine *FilesystemEngine, bucket, key, versionID string) (string, error) {
	t.Helper()
	r, _, err := engine.GetObject(context.Background(), bucket, key, versionID)
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s@%s: %v", key, versionID, err)
	}
	return string(b), nil
}

// bucketFiles lists the object data files a bucket holds on disk, in either
// layout, so tests can assert nothing was orphaned.
func bucketFiles(t *testing.T, engine *FilesystemEngine, bucket string) []string {
	t.Helper()
	return dataFiles(t, engine, bucket)
}

// TestEnablingVersioningPreservesExistingObject is the regression test for the
// object written before versioning was turned on. S3 keeps it as the "null"
// version; Stiva dropped it from metadata on the first versioned overwrite and
// left its file on disk where nothing could reach or delete it.
func TestEnablingVersioningPreservesExistingObject(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "pre-versioned"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "doc", "original")

	if err := engine.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "doc", "updated")

	if got, err := readVersion(t, engine, bucket, "doc", ""); err != nil || got != "updated" {
		t.Fatalf("latest = %q, %v; want %q", got, err, "updated")
	}
	got, err := readVersion(t, engine, bucket, "doc", "null")
	if err != nil || got != "original" {
		t.Fatalf("null version = %q, %v; want the pre-versioning content %q", got, err, "original")
	}

	out, err := engine.ListObjectVersions(&ListVersionsInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Versions) != 2 {
		t.Fatalf("ListObjectVersions returned %d versions, want 2 (the null version and the new one)", len(out.Versions))
	}

	// Deleting the null version must remove its file too.
	if _, _, err := engine.DeleteObject(bucket, "doc", "null"); err != nil {
		t.Fatal(err)
	}
	if files := bucketFiles(t, engine, bucket); len(files) != 1 {
		t.Errorf("after deleting the null version, bucket holds %v; want only the current version's file", files)
	}
}

// TestDeleteMarkerOverPreexistingObjectKeepsIt covers the delete path of the
// same bug: the first versioned DELETE of a pre-versioning object replaced its
// only record with a delete marker, losing the data.
func TestDeleteMarkerOverPreexistingObjectKeepsIt(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "pre-versioned-delete"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "doc", "original")
	if err := engine.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}

	isMarker, _, err := engine.DeleteObject(bucket, "doc", "")
	if err != nil || !isMarker {
		t.Fatalf("DeleteObject = marker %v, %v; want a delete marker", isMarker, err)
	}
	if got, err := readVersion(t, engine, bucket, "doc", "null"); err != nil || got != "original" {
		t.Fatalf("null version after delete marker = %q, %v; want %q", got, err, "original")
	}
}

// TestSuspendedVersioningReplacesPreexistingObject: with versioning suspended
// a write replaces the null version, and the pre-versioning object *is* the
// null version — so it must be replaced, not left behind as an orphan file.
func TestSuspendedVersioningReplacesPreexistingObject(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "pre-suspended"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "doc", "original")
	if err := engine.SetBucketVersioning(bucket, "Suspended"); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "doc", "replacement")

	if got, err := readVersion(t, engine, bucket, "doc", ""); err != nil || got != "replacement" {
		t.Fatalf("latest = %q, %v; want %q", got, err, "replacement")
	}
	out, err := engine.ListObjectVersions(&ListVersionsInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Versions) != 1 {
		t.Errorf("ListObjectVersions returned %d versions, want 1", len(out.Versions))
	}
	if files := bucketFiles(t, engine, bucket); len(files) != 1 {
		t.Errorf("bucket holds %v; want exactly one file", files)
	}
}

// TestVersioningCannotReturnToDisabled: S3 only allows Enabled and Suspended
// once a bucket has been versioned. Allowing "Disabled" made the next plain
// DELETE take the unversioned path, which erases every version of the key.
func TestVersioningCannotReturnToDisabled(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "no-going-back"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}

	// A bucket that was never versioned may be set to Disabled (a no-op).
	if err := engine.SetBucketVersioning(bucket, "Disabled"); err != nil {
		t.Fatalf("Disabled on a never-versioned bucket: %v", err)
	}

	for _, status := range []string{"Enabled", "Suspended"} {
		if err := engine.SetBucketVersioning(bucket, status); err != nil {
			t.Fatal(err)
		}
		err := engine.SetBucketVersioning(bucket, "Disabled")
		var s3Err *S3Error
		if !errors.As(err, &s3Err) || s3Err.Code != "IllegalVersioningConfigurationException" {
			t.Fatalf("Disabled after %s: err = %v, want IllegalVersioningConfigurationException", status, err)
		}
	}
}

// TestUnversionedOverwriteRemovesVersionFiles covers buckets left over from
// before Disabled was refused: an unversioned write drops the key's version
// history from metadata, and must drop the version files with it rather than
// leak them on disk forever.
func TestUnversionedOverwriteRemovesVersionFiles(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "legacy-disabled"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "doc", "v1")
	putString(t, engine, bucket, "doc", "v2")

	// Reproduce the legacy state directly: the engine no longer allows it.
	if err := engine.Metadata().PutBucketVersioning(bucket, "Disabled"); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "doc", "plain")

	if files := bucketFiles(t, engine, bucket); len(files) != 1 {
		t.Errorf("bucket holds %v; want only the unversioned object's file", files)
	}
	if got, err := readVersion(t, engine, bucket, "doc", ""); err != nil || got != "plain" {
		t.Errorf("doc reads %q, %v; want the unversioned write", got, err)
	}
}
