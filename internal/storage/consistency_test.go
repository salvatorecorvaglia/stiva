package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// checkKeyConsistent asserts the metadata and the data on disk agree for an
// unversioned key: a record whose data file exists with the recorded size, or
// no record — and in both cases no stray data files (the bucket holds only
// this key).
func checkKeyConsistent(t *testing.T, engine *FilesystemEngine, bucket, key string) {
	t.Helper()
	files := dataFiles(t, engine, bucket)
	meta, metaErr := engine.Metadata().GetObjectMeta(bucket, key, "")
	if metaErr != nil {
		if len(files) != 0 {
			t.Fatalf("no metadata for %s, but data files remain: %v", key, files)
		}
		return
	}
	path, err := engine.dataPath(meta)
	if err != nil {
		t.Fatal(err)
	}
	fi, statErr := os.Stat(path)
	switch {
	case statErr != nil:
		t.Fatalf("metadata for %s exists (size %d) but its file is missing", key, meta.Size)
	case meta.Size != fi.Size():
		t.Fatalf("metadata for %s says %d bytes, file on disk has %d", key, meta.Size, fi.Size())
	case len(files) != 1:
		t.Fatalf("%s: %d data files on disk, want exactly the one its record points at: %v", key, len(files), files)
	}
}

// TestConcurrentPutAndDeleteSameKeyStayConsistent is the regression test for
// DeleteObject not taking the per-key lock PutObject holds while it renames
// and writes metadata. A delete landing between a PUT's rename and its
// metadata write removed the new file, leaving metadata that pointed at
// nothing.
func TestConcurrentPutAndDeleteSameKeyStayConsistent(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "put-delete-race"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 200; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			body := strings.Repeat("p", 100+i)
			_, _ = engine.PutObject(context.Background(), bucket, "k", strings.NewReader(body), int64(len(body)), "application/octet-stream")
		}()
		go func() {
			defer wg.Done()
			_, _, _ = engine.DeleteObject(bucket, "k", "")
		}()
		wg.Wait()
		checkKeyConsistent(t, engine, bucket, "k")
	}
}

// TestConcurrentPutAndCompleteSameKeyStayConsistent: CompleteMultipartUpload
// renamed its assembled file into place and wrote metadata without the per-key
// lock, so a racing PUT could leave one request's bytes under the other's
// metadata.
func TestConcurrentPutAndCompleteSameKeyStayConsistent(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "put-complete-race"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		up, err := engine.CreateMultipartUpload(ctx, bucket, "k", "application/octet-stream")
		if err != nil {
			t.Fatal(err)
		}
		part := bytes.Repeat([]byte("m"), 3000)
		p, err := engine.UploadPart(ctx, bucket, "k", up.UploadID, 1, bytes.NewReader(part), int64(len(part)))
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = engine.CompleteMultipartUpload(ctx, bucket, "k", up.UploadID, []CompletePart{{PartNumber: 1, ETag: p.ETag}})
		}()
		go func() {
			defer wg.Done()
			body := strings.Repeat("p", 10)
			_, _ = engine.PutObject(ctx, bucket, "k", strings.NewReader(body), int64(len(body)), "application/octet-stream")
		}()
		wg.Wait()
		checkKeyConsistent(t, engine, bucket, "k")
	}
}

// TestDeleteBucketRacingPutNeverLosesAcknowledgedWrite is the regression test
// for DeleteBucket's check-then-act: it checked the bucket was empty, then
// removed the directory and metadata database. A PUT committing in between
// got a 200 while its object was destroyed along with the bucket.
func TestDeleteBucketRacingPutNeverLosesAcknowledgedWrite(t *testing.T) {
	engine := setupTestEngine(t)

	for i := 0; i < 100; i++ {
		bucket := fmt.Sprintf("racing-bucket-%d", i)
		if err := engine.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}

		var putErr, delErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, putErr = engine.PutObject(context.Background(), bucket, "obj", strings.NewReader("data"), 4, "application/octet-stream")
		}()
		go func() {
			defer wg.Done()
			delErr = engine.DeleteBucket(bucket)
		}()
		wg.Wait()

		if putErr == nil && delErr == nil {
			t.Fatalf("iteration %d: PutObject and DeleteBucket both succeeded — the acknowledged object was destroyed", i)
		}
		if putErr == nil {
			if _, err := engine.HeadObject(context.Background(), bucket, "obj", ""); err != nil {
				t.Fatalf("iteration %d: PutObject succeeded but the object is unreadable: %v", i, err)
			}
		}
	}
}

// TestBucketCacheRejectsStaleFill is the regression test for the bucket
// configuration cache being refilled with a value read before a concurrent
// update. With no expiry, a bucket switched to private could keep being
// served as public. The interleaving is reproduced deterministically: the
// read happens, the update lands, then the stale read tries to fill.
func TestBucketCacheRejectsStaleFill(t *testing.T) {
	engine := setupTestEngine(t)
	const bucket = "cache-race"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetBucketPublic(bucket, true); err != nil {
		t.Fatal(err)
	}

	m := engine.Metadata()
	m.invalidateBucketCache(bucket)

	gen := m.bucketCacheGeneration()
	stale, err := m.GetBucket(bucket) // IsPublic == true
	if err != nil {
		t.Fatal(err)
	}

	if err := engine.SetBucketPublic(bucket, false); err != nil {
		t.Fatal(err)
	}

	m.fillBucketCache(bucket, stale, nil, gen)

	public, err := engine.IsBucketPublic(bucket)
	if err != nil {
		t.Fatal(err)
	}
	if public {
		t.Fatal("bucket still reported public: a stale cache fill overwrote the update")
	}
}

// TestCreateBucketConcurrentOnlyOneSucceeds: the exists check and the create
// were separate steps, so two concurrent creates could both report success.
func TestCreateBucketConcurrentOnlyOneSucceeds(t *testing.T) {
	engine := setupTestEngine(t)

	for i := 0; i < 50; i++ {
		bucket := fmt.Sprintf("create-race-%d", i)
		errs := make([]error, 4)
		var wg sync.WaitGroup
		for j := range errs {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				errs[j] = engine.CreateBucket(bucket)
			}(j)
		}
		wg.Wait()

		ok := 0
		for _, err := range errs {
			var s3Err *S3Error
			switch {
			case err == nil:
				ok++
			case errors.As(err, &s3Err) && s3Err.Code == "BucketAlreadyOwnedByYou":
			default:
				t.Fatalf("unexpected CreateBucket error: %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: %d concurrent CreateBucket calls succeeded, want exactly 1", i, ok)
		}
	}
}

// TestFailedCommitReleasesBucketLock: lockBucketForCommit must release the
// shared bucket lock when the bucket turns out not to exist, or the next
// CreateBucket/DeleteBucket of that name waits forever.
func TestFailedCommitReleasesBucketLock(t *testing.T) {
	engine := setupTestEngine(t)
	if _, err := engine.lockBucketForCommit("missing-bucket"); err == nil {
		t.Fatal("lockBucketForCommit succeeded for a missing bucket")
	}

	acquired := make(chan struct{})
	go func() {
		unlock := engine.lockBucket("missing-bucket", true)
		unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the bucket lock was left held after a failed commit")
	}
}
