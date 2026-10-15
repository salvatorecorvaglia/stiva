package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dataFiles lists the object data files a bucket holds on disk, in either
// layout: blob files and legacy key-path files.
func dataFiles(t *testing.T, engine *FilesystemEngine, bucket string) []string {
	t.Helper()
	var files []string
	for _, root := range []string{engine.bucketPath(bucket), engine.blobDir(bucket)} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".stiva-tmp-") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func mustRead(t *testing.T, engine *FilesystemEngine, bucket, key, versionID string) string {
	t.Helper()
	got, err := readVersion(t, engine, bucket, key, versionID)
	if err != nil {
		t.Fatalf("read %s@%q: %v", key, versionID, err)
	}
	return got
}

// TestKeysThatCollidedOnDiskNowCoexist is the regression test for keys that
// the old layout could not store together, because each key was a path on the
// filesystem: "a" and "a/b" (a file cannot also be a directory), the folder
// markers other S3 tools create ("photos/" next to "photos/cat.jpg"), keys
// differing only in case on case-insensitive filesystems (macOS, Windows), and
// key segments longer than a filename may be. In S3 all of these are simply
// different keys.
func TestKeysThatCollidedOnDiskNowCoexist(t *testing.T) {
	engine := setupTestEngine(t)
	cases := map[string][]string{
		"file then nested":     {"a", "a/b"},
		"nested then file":     {"x/y", "x"},
		"folder marker":        {"photos/", "photos/cat.jpg"},
		"case differences":     {"Report.txt", "report.txt"},
		"long segment":         {strings.Repeat("s", 300) + "/data"},
		"dot segments in name": {"v1.2/notes", "v1.2"},
	}
	i := 0
	for name, keys := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			bucket := "coexist-" + string(rune('a'+i))
			if err := engine.CreateBucket(bucket); err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				putString(t, engine, bucket, key, "content of "+key)
			}
			for _, key := range keys {
				if got := mustRead(t, engine, bucket, key, ""); got != "content of "+key {
					t.Errorf("%q reads back %q", key, got)
				}
			}
		})
	}
}

// TestSuspendedNullVersionDoesNotCollideWithDottedKey: versions were stored
// at "<key>.<versionID>", so with versioning suspended a write to "a" (the
// "null" version, "a.null") overwrote the data of an unrelated key "a.null".
func TestSuspendedNullVersionDoesNotCollideWithDottedKey(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "dotted-keys"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "a.null", "an unrelated object")
	if err := engine.SetBucketVersioning(bucket, "Suspended"); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, bucket, "a", "the null version of a")

	if got := mustRead(t, engine, bucket, "a.null", ""); got != "an unrelated object" {
		t.Fatalf("a.null now reads %q: writing key a overwrote it", got)
	}
}

// TestReplacedDataIsRemoved: a write that replaces an object (an unversioned
// overwrite, or a new null version while suspended) used to rename over the
// old file. Each version now has its own file, so the replaced one must be
// deleted explicitly — and exactly the right ones.
func TestReplacedDataIsRemoved(t *testing.T) {
	engine := setupTestEngine(t)

	for _, versioning := range []string{"", "Suspended"} {
		bucket := "replace-plain"
		if versioning != "" {
			bucket = "replace-suspended"
		}
		if err := engine.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}
		if versioning != "" {
			if err := engine.SetBucketVersioning(bucket, versioning); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 3; i++ {
			putString(t, engine, bucket, "k", strings.Repeat("v", i+1))
		}
		if files := dataFiles(t, engine, bucket); len(files) != 1 {
			t.Errorf("%s: %d data files after three overwrites, want 1: %v", bucket, len(files), files)
		}
		if got := mustRead(t, engine, bucket, "k", ""); got != "vvv" {
			t.Errorf("%s: reads %q, want the last write", bucket, got)
		}
	}

	// Versioned: one file per version, removed with its version.
	const bucket = "replace-versioned"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	var versions []string
	for i := 0; i < 3; i++ {
		versions = append(versions, putString(t, engine, bucket, "k", strings.Repeat("v", i+1)).VersionID)
	}
	if files := dataFiles(t, engine, bucket); len(files) != 3 {
		t.Fatalf("%d data files for 3 versions, want 3", len(files))
	}
	if got := mustRead(t, engine, bucket, "k", versions[0]); got != "v" {
		t.Errorf("first version reads %q", got)
	}
	for _, v := range versions {
		if _, _, err := engine.DeleteObject(bucket, "k", v); err != nil {
			t.Fatal(err)
		}
	}
	if files := dataFiles(t, engine, bucket); len(files) != 0 {
		t.Errorf("%d data files left after deleting every version: %v", len(files), files)
	}
}

// TestLegacyLayoutObjectsStillWork: objects written before the blob layout
// stay where they are and are found through their metadata, which has no blob
// ID. Overwriting or deleting one removes its old file.
func TestLegacyLayoutObjectsStillWork(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "legacy-layout"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}

	writeLegacy := func(key, versionID, content string) {
		t.Helper()
		path, err := engine.objectPathWithVersion(bucket, key, versionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		info := &ObjectInfo{Bucket: bucket, Key: key, VersionID: versionID, Size: int64(len(content)),
			ETag: "legacy", ContentType: "text/plain", LastModified: time.Now().UTC(), IsLatest: true}
		if err := engine.Metadata().PutObjectMetaRaw(info); err != nil {
			t.Fatal(err)
		}
	}
	writeLegacy("docs/old.txt", "", "written by an older Stiva")

	if got := mustRead(t, engine, bucket, "docs/old.txt", ""); got != "written by an older Stiva" {
		t.Fatalf("legacy object reads %q", got)
	}

	putString(t, engine, bucket, "docs/old.txt", "rewritten")
	if got := mustRead(t, engine, bucket, "docs/old.txt", ""); got != "rewritten" {
		t.Fatalf("after overwrite reads %q", got)
	}
	if files := dataFiles(t, engine, bucket); len(files) != 1 {
		t.Errorf("overwriting a legacy object left %d data files: %v", len(files), files)
	}

	if _, _, err := engine.DeleteObject(bucket, "docs/old.txt", ""); err != nil {
		t.Fatal(err)
	}
	if files := dataFiles(t, engine, bucket); len(files) != 0 {
		t.Errorf("deleting left %d data files: %v", len(files), files)
	}
}

// TestKeyLengthLimit: with keys no longer bounded by filename limits, the S3
// limit of 1,024 bytes is enforced explicitly.
func TestKeyLengthLimit(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("long-keys"); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, "long-keys", strings.Repeat("k", maxKeyLength), "fits")

	_, err := engine.PutObject(context.Background(), "long-keys", strings.Repeat("k", maxKeyLength+1),
		strings.NewReader("x"), 1, "text/plain")
	var s3Err *S3Error
	if !errors.As(err, &s3Err) || s3Err.Code != "KeyTooLongError" {
		t.Fatalf("a %d-byte key: err = %v, want KeyTooLongError", maxKeyLength+1, err)
	}
}

// TestDeleteBucketRemovesItsBlobs: a deleted bucket leaves no data directory
// behind, in either layout.
func TestDeleteBucketRemovesItsBlobs(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("short-lived"); err != nil {
		t.Fatal(err)
	}
	putString(t, engine, "short-lived", "k", "x")
	if _, _, err := engine.DeleteObject("short-lived", "k", ""); err != nil {
		t.Fatal(err)
	}
	if err := engine.DeleteBucket("short-lived"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(engine.blobDir("short-lived")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the bucket's blob directory survived DeleteBucket (stat err = %v)", err)
	}
}
