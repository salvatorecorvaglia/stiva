package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// TestBucketDBEvictionDoesNotCloseInUseHandle is the regression test for the
// audit finding that LRU eviction closed a bbolt handle other goroutines were
// still using. Once more than maxOpenBucketDBs buckets were live, in-flight
// listings failed with "database not open".
//
// The sequence is deliberately deterministic: acquire a handle, then touch
// enough *other* buckets that the held one becomes the least-recently-used
// eviction candidate, and finally use it again.
func TestBucketDBEvictionDoesNotCloseInUseHandle(t *testing.T) {
	fs, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer fs.Close()

	// Enough buckets to force eviction several times over.
	total := maxOpenBucketDBs + 40
	names := make([]string, 0, total)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("bucket-%03d", i)
		if err := fs.CreateBucket(name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := fs.PutObject(context.Background(), name, "obj.txt",
			strings.NewReader("x"), 1, "application/octet-stream"); err != nil {
			t.Fatalf("put in %s: %v", name, err)
		}
		names = append(names, name)
	}

	victim := names[0]

	// Acquire first, so the held handle has the oldest lastUsed timestamp and is
	// therefore the prime eviction candidate for everything that follows.
	held, release, err := fs.Metadata().acquireBucketDB(victim)
	if err != nil {
		t.Fatalf("acquire held handle: %v", err)
	}
	defer release()

	// Sanity check: the handle works before any eviction pressure.
	if err := held.View(func(*bolt.Tx) error { return nil }); err != nil {
		t.Fatalf("held handle broken before eviction pressure: %v", err)
	}

	// Now drive eviction by touching every other bucket, sequentially so the
	// LRU ordering is unambiguous.
	for _, name := range names[1:] {
		if _, err := fs.ListObjects(&ListObjectsInput{Bucket: name, MaxKeys: 10}); err != nil {
			t.Fatalf("list %s: %v", name, err)
		}
	}

	// The held handle must still be usable. Before the refcount fix this failed
	// with bolt.ErrDatabaseNotOpen.
	if err := held.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(objectsBucket); b != nil {
			b.Stats()
		}
		return nil
	}); err != nil {
		t.Fatalf("held handle was closed by eviction while still referenced: %v", err)
	}
}

// TestBucketDBEvictionReclaimsIdleHandles confirms the refcount fix did not
// simply disable eviction: idle handles must still be reclaimed so the cap is
// meaningful.
func TestBucketDBEvictionReclaimsIdleHandles(t *testing.T) {
	fs, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer fs.Close()

	total := maxOpenBucketDBs + 25
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("idle-%03d", i)
		if err := fs.CreateBucket(name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := fs.ListObjects(&ListObjectsInput{Bucket: name, MaxKeys: 1}); err != nil {
			t.Fatalf("list %s: %v", name, err)
		}
	}

	open := fs.Metadata().activeBucketsCount()

	if open > maxOpenBucketDBs {
		t.Errorf("cache holds %d handles with none in use, want <= %d", open, maxOpenBucketDBs)
	}
}

// TestBucketDBConcurrentAcquireRelease hammers acquire/release on a single
// bucket to shake out refcount races under -race.
func TestBucketDBConcurrentAcquireRelease(t *testing.T) {
	fs, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer fs.Close()
	if err := fs.CreateBucket("hot"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 128)
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, release, err := fs.Metadata().acquireBucketDB("hot")
			if err != nil {
				errCh <- err
				return
			}
			defer release()
			if err := db.View(func(*bolt.Tx) error { return nil }); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("%v", err)
	}

	// After everything has been released the handle must be idle, not leaked.
	refCount, ok := fs.Metadata().activeBucketRefCount("hot")
	if !ok {
		t.Fatal("handle unexpectedly dropped from cache")
	}
	if refCount != 0 {
		t.Errorf("refCount = %d after all releases, want 0", refCount)
	}
}
