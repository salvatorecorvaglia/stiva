package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// UsageCounts measure logical payload bytes, before compression or encryption.
// Versions includes current versions and unversioned objects; delete markers
// have no payload and are counted separately.
type UsageCounts struct {
	Objects       int64 `json:"objects"`
	Versions      int64 `json:"versions"`
	DeleteMarkers int64 `json:"deleteMarkers"`
	LogicalBytes  int64 `json:"logicalBytes"`
	CurrentBytes  int64 `json:"currentBytes"`
	PreviousBytes int64 `json:"previousBytes"`
}

type BucketUsage struct {
	Name string `json:"name"`
	UsageCounts
}

type StorageUsage struct {
	UsageCounts
	Buckets        []BucketUsage `json:"buckets"`
	DiskTotalBytes uint64        `json:"diskTotalBytes"`
	DiskFreeBytes  uint64        `json:"diskFreeBytes"`
}

// CollectStorageUsage scans metadata without reading payloads or retaining an
// object list in memory. Each bucket is a consistent read transaction; buckets
// may change between transactions, so the overall result is a periodic snapshot.
func (fs *FilesystemEngine) CollectStorageUsage(ctx context.Context) (*StorageUsage, error) {
	buckets, err := fs.ListBuckets()
	if err != nil {
		return nil, err
	}
	out := &StorageUsage{Buckets: []BucketUsage{}}
	for _, bucket := range buckets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		counts, err := fs.metadata.bucketUsage(ctx, bucket.Name)
		if errors.Is(err, errNoSuchBucketRecord) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("collect bucket usage: %w", err)
		}
		out.Buckets = append(out.Buckets, BucketUsage{Name: bucket.Name, UsageCounts: counts})
		out.Objects += counts.Objects
		out.Versions += counts.Versions
		out.DeleteMarkers += counts.DeleteMarkers
		out.LogicalBytes += counts.LogicalBytes
		out.CurrentBytes += counts.CurrentBytes
		out.PreviousBytes += counts.PreviousBytes
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out.DiskTotalBytes, out.DiskFreeBytes, err = GetDiskSpace(fs.dataDir)
	if err != nil {
		return nil, fmt.Errorf("collect disk space: %w", err)
	}
	return out, nil
}

func (m *MetadataStore) bucketUsage(ctx context.Context, bucket string) (UsageCounts, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	var counts UsageCounts
	// Do not open/create a metadata database for a bucket removed during a scan.
	if err := m.globalDB.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketsBucket).Get([]byte(bucket)) == nil {
			return errNoSuchBucketRecord
		}
		return nil
	}); err != nil {
		return counts, err
	}
	db, release, err := m.acquireBucketDB(bucket)
	if err != nil {
		return counts, err
	}
	defer release()
	prefix := []byte(bucket + "\x00")
	err = db.View(func(tx *bolt.Tx) error {
		records := tx.Bucket(objectsBucket)
		if records == nil {
			return nil
		}
		cursor := records.Cursor()
		for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var obj ObjectInfo
			if err := json.Unmarshal(value, &obj); err != nil {
				return err
			}
			pointer := !bytes.ContainsRune(key[len(prefix):], '\x00')
			if pointer && !obj.IsDeleteMarker {
				counts.Objects++
				counts.CurrentBytes += obj.Size
			}
			// Versioned latest pointers duplicate a version record. Count the
			// version record once; unversioned objects only have a pointer.
			if pointer && obj.VersionID != "" {
				continue
			}
			if obj.IsDeleteMarker {
				counts.DeleteMarkers++
				continue
			}
			counts.Versions++
			counts.LogicalBytes += obj.Size
		}
		return nil
	})
	counts.PreviousBytes = counts.LogicalBytes - counts.CurrentBytes
	return counts, err
}

const UsageRefreshInterval = time.Minute

type UsageSnapshot struct {
	Usage      *StorageUsage `json:"usage"`
	UpdatedAt  *time.Time    `json:"updatedAt"`
	Refreshing bool          `json:"refreshing"`
	Error      string        `json:"error,omitempty"`
}

type UsageCollector interface {
	CollectStorageUsage(context.Context) (*StorageUsage, error)
}

// UsageTracker publishes completed snapshots. Requests only read the cache;
// a failed refresh retains the last successful snapshot and its timestamp.
type UsageTracker struct {
	source   UsageCollector
	mu       sync.RWMutex
	snapshot UsageSnapshot
}

func NewUsageTracker(source UsageCollector) *UsageTracker { return &UsageTracker{source: source} }

func (t *UsageTracker) Snapshot() UsageSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := t.snapshot
	if result.Usage != nil {
		usage := *result.Usage
		usage.Buckets = append([]BucketUsage{}, usage.Buckets...)
		result.Usage = &usage
	}
	if result.UpdatedAt != nil {
		stamp := *result.UpdatedAt
		result.UpdatedAt = &stamp
	}
	return result
}

func (t *UsageTracker) Refresh(ctx context.Context) error {
	t.mu.Lock()
	if t.snapshot.Refreshing {
		t.mu.Unlock()
		return nil
	}
	t.snapshot.Refreshing = true
	t.mu.Unlock()
	usage, err := t.source.CollectStorageUsage(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snapshot.Refreshing = false
	if err != nil {
		t.snapshot.Error = "Storage summary could not be refreshed; showing the last successful snapshot."
		return err
	}
	now := time.Now().UTC()
	t.snapshot.Usage, t.snapshot.UpdatedAt, t.snapshot.Error = usage, &now, ""
	return nil
}

func (t *UsageTracker) Run(ctx context.Context) {
	t.refresh(ctx)
	ticker := time.NewTicker(UsageRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.refresh(ctx)
		}
	}
}

func (t *UsageTracker) refresh(ctx context.Context) {
	if err := t.Refresh(ctx); err != nil && ctx.Err() == nil {
		slog.Error("[Storage] Failed to refresh usage summary", "error", err)
	}
}
