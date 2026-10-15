package storage

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// scanCount counts a bucket's current-version records the slow way, as the
// reference the maintained count must always agree with.
func scanCount(t *testing.T, m *MetadataStore, bucket string) int {
	t.Helper()
	n, err := m.countObjectMetasByScan(bucket)
	if err != nil {
		t.Fatalf("scan count: %v", err)
	}
	return n
}

// TestObjectCountStaysExactAcrossOperations drives a random mix of every
// operation that creates or removes a current-version record — plain and
// versioned writes, overwrites, deletes, delete markers, version deletes and
// multipart completions — and checks the maintained count against a full scan
// after each one.
//
// The count used to be recomputed by scanning the whole bucket whenever a
// write had invalidated its cache, which on a busy bucket meant a full scan
// on nearly every console dashboard load.
func TestObjectCountStaysExactAcrossOperations(t *testing.T) {
	engine := setupTestEngine(t)
	m := engine.Metadata()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))

	for _, versioning := range []string{"", "Enabled", "Suspended"} {
		bucket := "count-" + strings.ToLower(versioning)
		if versioning == "" {
			bucket = "count-plain"
		}
		if err := engine.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}
		if versioning != "" {
			if err := engine.SetBucketVersioning(bucket, versioning); err != nil {
				t.Fatal(err)
			}
		}

		for step := 0; step < 150; step++ {
			key := fmt.Sprintf("k%d", rng.Intn(8))
			var op string
			switch rng.Intn(5) {
			case 0, 1:
				op = "put"
				_, err := engine.PutObject(ctx, bucket, key, strings.NewReader("data"), 4, "application/octet-stream")
				if err != nil {
					t.Fatal(err)
				}
			case 2:
				op = "delete"
				if _, _, err := engine.DeleteObject(bucket, key, ""); err != nil {
					t.Fatal(err)
				}
			case 3:
				op = "delete-version"
				versions, _ := m.GetObjectVersions(bucket, key)
				if len(versions) > 0 {
					v := versions[rng.Intn(len(versions))]
					if _, _, err := engine.DeleteObject(bucket, key, v.VersionID); err != nil {
						t.Fatal(err)
					}
				}
			case 4:
				op = "multipart"
				up, err := engine.CreateMultipartUpload(ctx, bucket, key, "application/octet-stream")
				if err != nil {
					t.Fatal(err)
				}
				p, err := engine.UploadPart(ctx, bucket, key, up.UploadID, 1, strings.NewReader("part"), 4)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := engine.CompleteMultipartUpload(ctx, bucket, key, up.UploadID, []CompletePart{{PartNumber: 1, ETag: p.ETag}}); err != nil {
					t.Fatal(err)
				}
			}

			got, err := m.CountObjectMetas(bucket)
			if err != nil {
				t.Fatal(err)
			}
			if want := scanCount(t, m, bucket); got != want {
				t.Fatalf("%s bucket, step %d (%s %s): count = %d, scan = %d", bucket, step, op, key, got, want)
			}
		}

		if err := m.DeleteAllObjectMetas(bucket); err != nil {
			t.Fatal(err)
		}
		if got, _ := m.CountObjectMetas(bucket); got != 0 {
			t.Fatalf("%s: count after DeleteAllObjectMetas = %d", bucket, got)
		}
	}
}

// TestObjectCountBuiltForDatabasesWithoutOne: a bucket database from before
// counts were stored has none, and gets one computed by a single scan on
// first use — never a count trusted blindly.
func TestObjectCountBuiltForDatabasesWithoutOne(t *testing.T) {
	dir := t.TempDir()
	engine, err := NewFilesystemEngine(dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CreateBucket("legacy"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := engine.PutObject(context.Background(), "legacy", fmt.Sprintf("k%d", i), strings.NewReader("x"), 1, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}

	// Strip the stored count, as a pre-upgrade database would have none.
	db, release, err := engine.Metadata().acquireBucketDB("legacy")
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(statsBucket) })
	release()
	if err != nil {
		t.Fatal(err)
	}

	// Writes before the first count must not invent one from nothing.
	if _, err := engine.PutObject(context.Background(), "legacy", "k9", strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if got, _ := engine.Metadata().CountObjectMetas("legacy"); got != 6 {
		t.Fatalf("count for a database without a stored count = %d, want 6", got)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	// And it persists across a restart.
	reopened, err := NewFilesystemEngine(dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, _ := reopened.Metadata().CountObjectMetas("legacy"); got != 6 {
		t.Fatalf("count after restart = %d, want 6", got)
	}
}
