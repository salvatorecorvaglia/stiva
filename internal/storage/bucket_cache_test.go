package storage

import (
	"testing"
)

// TestBucketExistsIsServedFromCache is the regression test for every request
// paying for a database transaction just to learn that its bucket exists:
// nearly every engine operation, and the router's per-request logging and
// public-access checks, called BucketExists, which went to bbolt each time.
// Closing the database underneath proves a warm lookup never touches it.
func TestBucketExistsIsServedFromCache(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("cached"); err != nil {
		t.Fatal(err)
	}
	m := engine.Metadata()

	if ok, err := m.BucketExists("cached"); err != nil || !ok {
		t.Fatalf("BucketExists = %v, %v", ok, err)
	}
	if ok, err := m.BucketExists("absent"); err != nil || ok {
		t.Fatalf("BucketExists(absent) = %v, %v", ok, err)
	}

	if err := m.globalDB.Close(); err != nil {
		t.Fatal(err)
	}

	if ok, err := m.BucketExists("cached"); err != nil || !ok {
		t.Errorf("warm BucketExists hit the database: %v, %v", ok, err)
	}
	if ok, err := m.BucketExists("absent"); err != nil || ok {
		t.Errorf("warm negative BucketExists hit the database: %v, %v", ok, err)
	}

	// A lookup that genuinely fails must say so, not be cached as "absent".
	if ok, err := m.BucketExists("never-seen"); err == nil {
		t.Errorf("BucketExists with the database closed = %v, nil; want the read error", ok)
	}
	if _, cached := m.bucketCache["never-seen"]; cached {
		t.Error("a failed read was cached as a missing bucket")
	}
}

func TestBucketExistsTracksCreateAndDelete(t *testing.T) {
	engine := setupTestEngine(t)
	m := engine.Metadata()
	check := func(want bool) {
		t.Helper()
		if ok, err := m.BucketExists("cycle"); err != nil || ok != want {
			t.Fatalf("BucketExists = %v, %v; want %v", ok, err, want)
		}
	}

	check(false)
	if err := engine.CreateBucket("cycle"); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err := engine.DeleteBucket("cycle"); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := engine.CreateBucket("cycle"); err != nil {
		t.Fatal(err)
	}
	check(true)
}
