package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func benchmarkEngine(b *testing.B) *FilesystemEngine {
	b.Helper()
	engine, err := NewFilesystemEngine(b.TempDir(), nil, "")
	if err != nil {
		b.Fatal(err)
	}
	engine.SetMetrics(&MetricsTracker{})
	b.Cleanup(func() {
		if err := engine.Close(); err != nil {
			b.Error(err)
		}
	})
	if err := engine.CreateBucket("benchmark"); err != nil {
		b.Fatal(err)
	}
	return engine
}

// Worker counts are absolute, rather than multipliers of GOMAXPROCS. Reusing
// one key per worker bounds disk growth while exercising independent writes.
func BenchmarkConcurrentPublication(b *testing.B) {
	for _, versioned := range []bool{false, true} {
		for _, contended := range []bool{false, true} {
			for _, workers := range []int{1, 4, 16} {
				b.Run(fmt.Sprintf("versioned-%t/same-key-%t/workers-%d", versioned, contended, workers), func(b *testing.B) {
					engine := benchmarkEngine(b)
					if versioned {
						if err := engine.SetBucketVersioning("benchmark", "Enabled"); err != nil {
							b.Fatal(err)
						}
					}
					payloads := make([][]byte, workers)
					for i := range payloads {
						payloads[i] = bytes.Repeat([]byte{byte(i)}, 64<<10)
					}
					var sequence atomic.Int64
					var group sync.WaitGroup
					b.SetBytes(64 << 10)
					b.ReportAllocs()
					b.ResetTimer()
					for worker := 0; worker < workers; worker++ {
						group.Add(1)
						go func(worker int) {
							defer group.Done()
							key := fmt.Sprintf("worker-%d", worker)
							if contended {
								key = "shared"
							}
							for sequence.Add(1) <= int64(b.N) {
								if _, err := engine.PutObject(context.Background(), "benchmark", key, bytes.NewReader(payloads[worker]), int64(len(payloads[worker])), "application/octet-stream"); err != nil {
									b.Error(err)
									return
								}
							}
						}(worker)
					}
					group.Wait()
					b.StopTimer()
				})
			}
		}
	}
}

// These metadata-only fixtures do not claim payload I/O throughput. One bulk
// transaction makes large listing/usage fixtures reproducible without spending
// minutes fsyncing setup writes outside the measured region.
func BenchmarkLargeBucketMetadata(b *testing.B) {
	for _, keys := range []int{1000, 10000} {
		for _, operation := range []string{"listing", "prefix", "versions", "usage"} {
			b.Run(fmt.Sprintf("keys-%d/%s", keys, operation), func(b *testing.B) {
				engine := benchmarkEngine(b)
				db, release, err := engine.metadata.acquireBucketDB("benchmark")
				if err != nil {
					b.Fatal(err)
				}
				err = db.Update(func(tx *bolt.Tx) error {
					records := tx.Bucket(objectsBucket)
					for i := 0; i < keys; i++ {
						for version := 0; version < 4; version++ {
							info := ObjectInfo{Bucket: "benchmark", Key: fmt.Sprintf("folder-%03d/item-%08d", i%100, i), VersionID: fmt.Sprint(version), Size: 64 << 10, ContentType: "application/octet-stream", LastModified: time.Unix(1700000000+int64(version), 0), IsLatest: version == 3}
							raw, err := json.Marshal(info)
							if err != nil {
								return err
							}
							if err := records.Put(objectMetaKeyVersion(info.Bucket, info.Key, info.VersionID), raw); err != nil {
								return err
							}
							if info.IsLatest {
								if err := records.Put(objectMetaKey(info.Bucket, info.Key), raw); err != nil {
									return err
								}
							}
						}
					}
					return setObjectCount(tx, uint64(keys))
				})
				release()
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for b.Loop() {
					switch operation {
					case "listing":
						_, err = engine.ListObjects(&ListObjectsInput{Bucket: "benchmark", MaxKeys: 50})
					case "prefix":
						_, err = engine.ListObjects(&ListObjectsInput{Bucket: "benchmark", Prefix: "folder-075/", MaxKeys: 50})
					case "versions":
						_, err = engine.ListObjectVersions(&ListVersionsInput{Bucket: "benchmark", MaxKeys: 50, KeyMarker: "folder-050/"})
					case "usage":
						_, err = engine.CollectStorageUsage(context.Background())
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
