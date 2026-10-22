package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var replicationBucket = []byte("replication-pending")

// markReplication is part of the object's metadata transaction: either both
// commit, or neither does. Revisions prevent an old delivery clearing new work.
func (m *MetadataStore) markReplication(tx *bolt.Tx, key string) error {
	if !m.replication {
		return nil
	}
	b, err := tx.CreateBucketIfNotExists(replicationBucket)
	if err != nil {
		return err
	}
	seq, err := b.NextSequence()
	if err != nil {
		return err
	}
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], seq)
	return b.Put([]byte(key), revision[:])
}

type pendingReplica struct {
	key      string
	revision []byte
}

// StartDurableReplication must be called before serving requests. It selects
// the durable reconciler in place of the best-effort channel dispatcher.
func (fs *FilesystemEngine) StartDurableReplication() {
	if fs.syncConfig == nil {
		return
	}
	fs.durableOnce.Do(func() {
		fs.durableReplication = true
		fs.durableStop = make(chan struct{})
		fs.syncWG.Add(1)
		go func() {
			defer fs.syncWG.Done()
			// Backfill pre-existing objects on every start. Existing tombstones stay
			// pending; only a successful remote request acknowledges them.
			seeded := false
			timer := time.NewTicker(5 * time.Second)
			defer timer.Stop()
			for {
				select {
				case <-fs.durableStop:
					return
				default:
				}
				if !seeded {
					if err := fs.seedReplication(); err != nil {
						slog.Error("[Sync] Backfill failed; retrying next scan", "error", err)
						fs.Metrics().IncSyncFailure()
					} else {
						seeded = true
					}
				}
				if err := fs.reconcilePending(fs.dispatchContext()); err != nil {
					slog.Error("[Sync] Reconciliation failed", "error", err)
					fs.Metrics().IncSyncFailure()
				}
				select {
				case <-fs.durableStop:
					return
				case <-timer.C:
				}
			}
		}()
	})
}

func (fs *FilesystemEngine) seedReplication() error {
	buckets, err := fs.metadata.ListBuckets()
	if err != nil {
		return err
	}
	for _, bucket := range buckets {
		db, release, err := fs.metadata.acquireBucketDB(bucket.Name)
		if err != nil {
			return err
		}
		err = db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(objectsBucket).ForEach(func(_, v []byte) error {
				var info ObjectInfo
				if err := json.Unmarshal(v, &info); err != nil {
					return err
				}
				if info.IsLatest {
					return fs.metadata.markReplication(tx, info.Key)
				}
				return nil
			})
		})
		release()
		if err != nil {
			return err
		}
	}
	return nil
}

func (fs *FilesystemEngine) reconcilePending(ctx context.Context) error {
	buckets, err := fs.metadata.ListBuckets()
	if err != nil {
		return err
	}
	var total int64
	for _, bucket := range buckets {
		db, release, err := fs.metadata.acquireBucketDB(bucket.Name)
		if err != nil {
			return err
		}
		err = db.View(func(tx *bolt.Tx) error {
			if b := tx.Bucket(replicationBucket); b != nil {
				total += int64(b.Stats().KeyN)
			}
			return nil
		})
		release()
		if err != nil {
			return err
		}
	}
	atomic.StoreInt64(&fs.Metrics().SyncPending, total)
	for _, bucket := range buckets {
		var after []byte
		for {
			select {
			case <-fs.durableStop:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			db, release, err := fs.metadata.acquireBucketDB(bucket.Name)
			if err != nil {
				return err
			}
			var batch []pendingReplica
			err = db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket(replicationBucket)
				if b == nil {
					return nil
				}
				c := b.Cursor()
				k, v := c.First()
				if after != nil {
					k, v = c.Seek(after)
					if bytes.Equal(k, after) {
						k, v = c.Next()
					}
				}
				for ; k != nil && len(batch) < 100; k, v = c.Next() {
					batch = append(batch, pendingReplica{string(k), bytes.Clone(v)})
				}
				return nil
			})
			release()
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			for _, item := range batch {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// No other remote delivery runs concurrently. Holding the object lock
				// keeps GET metadata/data consistent throughout the transfer.
				unlock := fs.lockObjectKey(bucket.Name, item.key)
				err = fs.deliverPending(ctx, bucket.Name, item)
				unlock()
				if err != nil {
					fs.Metrics().IncSyncFailure()
					slog.Error("[Sync] Pending object retained for retry", "bucket", bucket.Name, "key", item.key, "error", err)
				}
			}
			after = []byte(batch[len(batch)-1].key)
		}
	}
	atomic.StoreInt64(&fs.Metrics().SyncLastScan, time.Now().Unix())
	return nil
}

func (fs *FilesystemEngine) deliverPending(ctx context.Context, bucket string, item pendingReplica) error {
	db, release, err := fs.metadata.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer release()
	op := "DELETE"
	err = db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(objectsBucket).Get(objectMetaKey(bucket, item.key))
		if data == nil {
			return nil
		}
		var info ObjectInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		if !info.IsDeleteMarker {
			op = "PUT"
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := performSync(ctx, fs, fs.syncClient, fs.syncConfig, bucket, item.key, op); err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(replicationBucket)
		if b == nil {
			return errors.New("replication journal missing")
		}
		if bytes.Equal(b.Get([]byte(item.key)), item.revision) {
			return b.Delete([]byte(item.key))
		}
		return nil
	})
}

// CheckReady tests database reads and durable writes on the storage volume.
func (fs *FilesystemEngine) CheckReady() error {
	fs.readyMu.Lock()
	defer fs.readyMu.Unlock()
	if time.Since(fs.readyAt) < time.Second {
		return fs.readyErr
	}
	fs.readyErr = fs.checkReady()
	fs.readyAt = time.Now()
	return fs.readyErr
}

func (fs *FilesystemEngine) checkReady() error {
	if _, err := fs.metadata.ListBuckets(); err != nil {
		return fmt.Errorf("metadata unavailable: %w", err)
	}
	return fs.probeDisk()
}
