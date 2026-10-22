package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStorageUsageCountsVersionsAndMarkersOnce(t *testing.T) {
	engine, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	ctx := context.Background()
	for _, name := range []string{"plain", "versioned", "empty"} {
		if err := engine.CreateBucket(name); err != nil {
			t.Fatal(err)
		}
	}
	put := func(bucket, key, body string) *ObjectInfo {
		t.Helper()
		info, err := engine.PutObject(ctx, bucket, key, strings.NewReader(body), int64(len(body)), "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	put("plain", "same", "old")
	put("plain", "same", "fresh")
	if err := engine.SetBucketVersioning("versioned", "Enabled"); err != nil {
		t.Fatal(err)
	}
	oldest := put("versioned", "a", "old")
	put("versioned", "a", "newer")
	if _, _, err := engine.DeleteObject("versioned", "a", ""); err != nil {
		t.Fatal(err)
	}
	put("versioned", "b", "ok")
	usage, err := engine.CollectStorageUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := UsageCounts{Objects: 2, Versions: 4, DeleteMarkers: 1, LogicalBytes: 15, CurrentBytes: 7, PreviousBytes: 8}
	if usage.UsageCounts != want {
		t.Fatalf("counts = %+v, want %+v", usage.UsageCounts, want)
	}
	if len(usage.Buckets) != 3 {
		t.Fatalf("buckets: %+v", usage.Buckets)
	}
	if usage.DiskTotalBytes == 0 || usage.DiskFreeBytes > usage.DiskTotalBytes {
		t.Fatalf("disk: %+v", usage)
	}
	for _, bucket := range usage.Buckets {
		if bucket.Name == "empty" && bucket.UsageCounts != (UsageCounts{}) {
			t.Fatalf("empty bucket: %+v", bucket)
		}
		if bucket.Name == "versioned" && bucket.LogicalBytes != 10 {
			t.Fatalf("versioned bucket: %+v", bucket)
		}
	}
	if err := engine.SetBucketVersioning("versioned", "Suspended"); err != nil {
		t.Fatal(err)
	}
	put("versioned", "a", "null")
	put("versioned", "a", "latest") // replaces the null version, not an extra version
	if _, _, err := engine.DeleteObject("versioned", "a", oldest.VersionID); err != nil {
		t.Fatal(err)
	}
	usage, err = engine.CollectStorageUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want = UsageCounts{Objects: 3, Versions: 4, DeleteMarkers: 1, LogicalBytes: 18, CurrentBytes: 13, PreviousBytes: 5}
	if usage.UsageCounts != want {
		t.Fatalf("after suspension: %+v, want %+v", usage.UsageCounts, want)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := engine.CollectStorageUsage(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

type usageSourceFunc func(context.Context) (*StorageUsage, error)

func (f usageSourceFunc) CollectStorageUsage(ctx context.Context) (*StorageUsage, error) {
	return f(ctx)
}

func TestUsageTrackerPreservesLastGoodSnapshot(t *testing.T) {
	fail := false
	tracker := NewUsageTracker(usageSourceFunc(func(context.Context) (*StorageUsage, error) {
		if fail {
			return nil, errors.New("private filesystem error")
		}
		return &StorageUsage{Buckets: []BucketUsage{{Name: "first"}}}, nil
	}))
	if tracker.Snapshot().Usage != nil {
		t.Fatal("unexpected initial snapshot")
	}
	if err := tracker.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := tracker.Snapshot()
	if first.Usage == nil || first.UpdatedAt == nil || first.Refreshing {
		t.Fatalf("first: %+v", first)
	}
	fail = true
	if err := tracker.Refresh(context.Background()); err == nil {
		t.Fatal("refresh should fail")
	}
	failed := tracker.Snapshot()
	if failed.Usage == nil || !failed.UpdatedAt.Equal(*first.UpdatedAt) || failed.Error == "" || strings.Contains(failed.Error, "private") {
		t.Fatalf("failed: %+v", failed)
	}
	first.Usage.Buckets[0].Name = "mutated"
	if tracker.Snapshot().Usage.Buckets[0].Name != "first" {
		t.Fatal("snapshot aliases mutable cache")
	}
	fail = false
	if err := tracker.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tracker.Snapshot().Error != "" {
		t.Fatal("error not cleared after recovery")
	}
}

func TestUsageTrackerCancelsInFlightScan(t *testing.T) {
	entered := make(chan struct{})
	tracker := NewUsageTracker(usageSourceFunc(func(ctx context.Context) (*StorageUsage, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); tracker.Run(ctx) }()
	<-entered
	if !tracker.Snapshot().Refreshing {
		t.Fatal("in-flight scan is not marked refreshing")
	}
	cancel()
	<-done
	if tracker.Snapshot().Refreshing {
		t.Fatal("scan still refreshing after cancellation")
	}
}
