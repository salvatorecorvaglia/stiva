package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func corruptRecord(t *testing.T, engine *FilesystemEngine, bucket string, collection, key []byte) {
	t.Helper()
	db, release, err := engine.metadata.acquireBucketDB(bucket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket(collection).Put(key, []byte("{")) }); err != nil {
		t.Fatal(err)
	}
}

func requireInternalError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("storage failure was ignored")
	}
	var s3err *S3Error
	if errors.As(err, &s3err) {
		t.Fatalf("internal corruption was misreported as %s", s3err.Code)
	}
}

func TestCorruptCurrentMetadataFailsWithoutMutation(t *testing.T) {
	for _, status := range []string{"Disabled", "Enabled", "Suspended"} {
		t.Run(status, func(t *testing.T) {
			engine := setupTestEngine(t)
			const bucket = "corrupt-current"
			if err := engine.CreateBucket(bucket); err != nil {
				t.Fatal(err)
			}
			if err := engine.SetBucketVersioning(bucket, status); err != nil {
				t.Fatal(err)
			}
			info, err := engine.PutObject(context.Background(), bucket, "key", strings.NewReader("original"), 8, "text/plain")
			if err != nil {
				t.Fatal(err)
			}
			path, err := engine.dataPath(info)
			if err != nil {
				t.Fatal(err)
			}
			corruptRecord(t, engine, bucket, objectsBucket, objectMetaKey(bucket, "key"))
			_, err = engine.HeadObject(context.Background(), bucket, "key", "")
			requireInternalError(t, err)
			_, _, err = engine.GetObject(context.Background(), bucket, "key", "")
			requireInternalError(t, err)
			_, _, err = engine.DeleteObject(bucket, "key", "")
			requireInternalError(t, err)
			_, err = engine.PutObject(context.Background(), bucket, "key", strings.NewReader("new"), 3, "text/plain")
			requireInternalError(t, err)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("failed operation removed the original blob: %v", err)
			}
		})
	}
}

func TestCorruptHistoryRollsBackVersionDeletion(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "corrupt-history"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	first, err := engine.PutObject(context.Background(), bucket, "key", strings.NewReader("first"), 5, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	current, err := engine.PutObject(context.Background(), bucket, "key", strings.NewReader("current"), 7, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	corruptRecord(t, engine, bucket, objectsBucket, objectMetaKeyVersion(bucket, "key", first.VersionID))
	_, _, err = engine.DeleteObject(bucket, "key", current.VersionID)
	requireInternalError(t, err)
	remaining, err := engine.HeadObject(context.Background(), bucket, "key", "")
	if err != nil {
		t.Fatal(err)
	}
	if remaining.VersionID != current.VersionID {
		t.Fatal("failed transaction changed the current pointer")
	}
	_, err = engine.metadata.GetObjectVersions(bucket, "key")
	requireInternalError(t, err)
}

func TestCorruptMultipartMetadataIsNotMissingUpload(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "corrupt-multipart"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	upload, err := engine.CreateMultipartUpload(context.Background(), bucket, "key", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	corruptRecord(t, engine, bucket, multipartBucket, multipartKey(bucket, "key", upload.UploadID))
	_, err = engine.UploadPart(context.Background(), bucket, "key", upload.UploadID, 1, strings.NewReader("x"), 1)
	requireInternalError(t, err)
	_, err = engine.CompleteMultipartUpload(context.Background(), bucket, "key", upload.UploadID, nil)
	requireInternalError(t, err)
	_, err = engine.ListParts(bucket, "key", upload.UploadID)
	requireInternalError(t, err)
	requireInternalError(t, engine.AbortMultipartUpload(bucket, "key", upload.UploadID))
	requireInternalError(t, engine.CleanExpiredMultipartUploads(context.Background(), time.Hour))
	if err := engine.PutBucketLifecycle(bucket, &LifecycleConfiguration{Rules: []LifecycleRule{{ID: "abort", Status: "Enabled", AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: 1}}}}); err != nil {
		t.Fatal(err)
	}
	requireInternalError(t, engine.CleanExpiredObjects(context.Background()))
}

func TestMultipartCleanupFailureRemainsRetryable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX permission enforcement for failure injection")
	}
	for _, lifecycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "global", true: "lifecycle"}[lifecycle], func(t *testing.T) {
			engine := setupTestEngine(t)
			const bucket = "retry-cleanup"
			if err := engine.CreateBucket(bucket); err != nil {
				t.Fatal(err)
			}
			upload, err := engine.CreateMultipartUpload(context.Background(), bucket, "key", "text/plain")
			if err != nil {
				t.Fatal(err)
			}
			meta, err := engine.metadata.GetMultipartMeta(bucket, "key", upload.UploadID)
			if err != nil {
				t.Fatal(err)
			}
			meta.Created = time.Now().Add(-48 * time.Hour)
			if err := engine.metadata.PutMultipartMeta(meta); err != nil {
				t.Fatal(err)
			}
			if err := engine.PutBucketLifecycle(bucket, &LifecycleConfiguration{Rules: []LifecycleRule{{ID: "abort", Status: "Enabled", AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: 1}}}}); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(engine.dataDir, "multipart", bucket)
			if err := os.Chmod(parent, 0500); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(parent, 0755)
			if lifecycle {
				err = engine.CleanExpiredObjects(context.Background())
			} else {
				err = engine.CleanExpiredMultipartUploads(context.Background(), time.Hour)
			}
			requireInternalError(t, err)
			if _, err := engine.metadata.GetMultipartMeta(bucket, "key", upload.UploadID); err != nil {
				t.Fatalf("failed cleanup lost retry metadata: %v", err)
			}
			if err := os.Chmod(parent, 0755); err != nil {
				t.Fatal(err)
			}
			if err := engine.CleanExpiredMultipartUploads(context.Background(), time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.metadata.GetMultipartMeta(bucket, "key", upload.UploadID); !errors.Is(err, errMultipartMetaNotFound) {
				t.Fatalf("retry did not complete: %v", err)
			}
		})
	}
}
