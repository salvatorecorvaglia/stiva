package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestMetadataMigration(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Manually build a legacy database with all metadata in one file
	legacyPath := filepath.Join(tmpDir, "stiva.db")
	db, err := bolt.Open(legacyPath, 0600, nil)
	if err != nil {
		t.Fatalf("failed to open legacy db: %v", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		// Create legacy buckets
		bBucket, err := tx.CreateBucket(bucketsBucket)
		if err != nil {
			return err
		}
		bucket1 := BucketInfo{Name: "b1", CreationDate: time.Now().UTC()}
		bData, _ := json.Marshal(bucket1)
		_ = bBucket.Put([]byte("b1"), bData)

		bucket2 := BucketInfo{Name: "b2", CreationDate: time.Now().UTC()}
		bData2, _ := json.Marshal(bucket2)
		_ = bBucket.Put([]byte("b2"), bData2)

		// Create legacy objects bucket
		oBucket, err := tx.CreateBucket(objectsBucket)
		if err != nil {
			return err
		}
		obj1 := ObjectInfo{Bucket: "b1", Key: "file1.txt", Size: 10, ETag: "etag1", LastModified: time.Now().UTC()}
		oData1, _ := json.Marshal(obj1)
		_ = oBucket.Put([]byte("b1\x00file1.txt"), oData1)

		obj2 := ObjectInfo{Bucket: "b2", Key: "file2.txt", Size: 20, ETag: "etag2", LastModified: time.Now().UTC()}
		oData2, _ := json.Marshal(obj2)
		_ = oBucket.Put([]byte("b2\x00file2.txt"), oData2)

		// Create legacy multipart bucket
		mBucket, err := tx.CreateBucket(multipartBucket)
		if err != nil {
			return err
		}
		mp1 := MultipartMeta{UploadID: "up1", Bucket: "b1", Key: "large.bin", Created: time.Now().UTC()}
		mData1, _ := json.Marshal(mp1)
		_ = mBucket.Put([]byte("b1\x00large.bin\x00up1"), mData1)

		return nil
	})
	if err != nil {
		db.Close()
		t.Fatalf("failed to populate legacy db: %v", err)
	}
	db.Close()

	// 2. Open the MetadataStore (which triggers auto-migration)
	meta, err := NewMetadataStore(tmpDir)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer meta.Close()

	// 3. Verify that the files were partitioned correctly
	if _, err := os.Stat(filepath.Join(tmpDir, "metadata", "b1.db")); os.IsNotExist(err) {
		t.Error("expected b1.db to exist in metadata directory")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "metadata", "b2.db")); os.IsNotExist(err) {
		t.Error("expected b2.db to exist in metadata directory")
	}

	// 4. Verify legacy objects & multipart buckets were cleaned up from global database
	err = meta.globalDB.View(func(tx *bolt.Tx) error {
		if tx.Bucket(objectsBucket) != nil {
			t.Error("objects bucket was not removed from central database")
		}
		if tx.Bucket(multipartBucket) != nil {
			t.Error("multipart bucket was not removed from central database")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("globalDB verification failed: %v", err)
	}

	// 5. Verify data retrieval from new partitioned stores
	o1, err := meta.GetObjectMeta("b1", "file1.txt", "")
	if err != nil {
		t.Fatalf("failed to retrieve b1 object: %v", err)
	}
	if o1.Size != 10 || o1.ETag != "etag1" {
		t.Errorf("unexpected obj1 attributes: %+v", o1)
	}

	o2, err := meta.GetObjectMeta("b2", "file2.txt", "")
	if err != nil {
		t.Fatalf("failed to retrieve b2 object: %v", err)
	}
	if o2.Size != 20 || o2.ETag != "etag2" {
		t.Errorf("unexpected obj2 attributes: %+v", o2)
	}

	mp, err := meta.GetMultipartMeta("b1", "large.bin", "up1")
	if err != nil {
		t.Fatalf("failed to retrieve b1 multipart meta: %v", err)
	}
	if mp.UploadID != "up1" {
		t.Errorf("unexpected multipart meta: %+v", mp)
	}
}

func TestMetadataInitLocksCleanupOnError(t *testing.T) {
	tmpDir := t.TempDir()

	meta, err := NewMetadataStore(tmpDir)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer meta.Close()

	// Pre-create a directory at the expected dbPath to force bolt.Open to fail
	badBucket := "bad-bucket"
	dbPath := filepath.Join(tmpDir, "metadata", badBucket+".db")
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		t.Fatalf("failed to create directory to block db: %v", err)
	}

	// Trigger getBucketDB via GetObjectMeta
	_, err = meta.GetObjectMeta(badBucket, "some-key", "")
	if err == nil {
		t.Fatal("expected GetObjectMeta to fail when DB path is a directory")
	}

	// Verify that the badBucket lock is cleaned up from initLocks map
	if meta.hasInitLock(badBucket) {
		t.Error("expected initLocks entry to be deleted after initialization failure")
	}
}

func TestListBucketsIgnoresSystemKeys(t *testing.T) {
	tmpDir := t.TempDir()

	meta, err := NewMetadataStore(tmpDir)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer meta.Close()

	// 1. Create a regular bucket
	bucket := &BucketInfo{
		Name:         "testbucket",
		CreationDate: time.Now().UTC(),
	}
	if err := meta.PutBucket(bucket); err != nil {
		t.Fatalf("failed to put bucket: %v", err)
	}

	// 2. Put a system value (which creates a key starting with _sys_)
	if err := meta.PutSystemValue("test_key", "raw-hex-non-json-value"); err != nil {
		t.Fatalf("failed to put system value: %v", err)
	}

	// 3. Call ListBuckets and verify that only the regular bucket is returned and no error occurs
	buckets, err := meta.ListBuckets()
	if err != nil {
		t.Fatalf("ListBuckets failed: %v", err)
	}

	if len(buckets) != 1 {
		t.Errorf("expected exactly 1 bucket, got %d", len(buckets))
	}

	if buckets[0].Name != "testbucket" {
		t.Errorf("expected bucket name 'testbucket', got '%s'", buckets[0].Name)
	}
}

func TestConcurrentGetBucketDBInitLocks(t *testing.T) {
	tmpDir := t.TempDir()

	meta, err := NewMetadataStore(tmpDir)
	if err != nil {
		t.Fatalf("NewMetadataStore failed: %v", err)
	}
	defer meta.Close()

	badBucket := "concurrent-bad-bucket"
	dbPath := filepath.Join(tmpDir, "metadata", badBucket+".db")
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		t.Fatalf("failed to create directory to block db: %v", err)
	}

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			_, _ = meta.GetObjectMeta(badBucket, "some-key", "")
		}()
	}

	wg.Wait()

	// Verify that the entry in initLocks is eventually deleted after all goroutines complete
	if meta.hasInitLock(badBucket) {
		t.Error("expected initLocks entry to be deleted after all concurrent attempts complete")
	}
}
