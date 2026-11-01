package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

var errObjectMetaNotFound = errors.New("object not found")

func objectMetaKey(bucket, key string) []byte {
	return []byte(bucket + "\x00" + key)
}

func objectMetaKeyVersion(bucket, key, versionID string) []byte {
	return []byte(bucket + "\x00" + key + "\x00" + versionID)
}

// PutObjectMeta stores object metadata (both the latest pointer and the historical record).
func (m *MetadataStore) PutObjectMeta(info *ObjectInfo) error {
	unlock := m.acquireBucketLock(info.Bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(info.Bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
		if err := m.markReplication(tx, info.Key); err != nil {
			return err
		}
		b := tx.Bucket(objectsBucket)

		info.IsLatest = true

		// Update old latest version if present
		oldData := b.Get(objectMetaKey(info.Bucket, info.Key))
		if oldData == nil {
			if err := adjustObjectCount(tx, 1); err != nil {
				return err
			}
		}
		if oldData != nil {
			var oldInfo ObjectInfo
			if err := json.Unmarshal(oldData, &oldInfo); err != nil {
				return err
			}
			if oldInfo.VersionID != "" && oldInfo.VersionID != info.VersionID {
				oldInfo.IsLatest = false
				oldInfo.NoncurrentSince = info.LastModified
				oldHistData, err := json.Marshal(oldInfo)
				if err != nil {
					return err
				}
				if err := b.Put(objectMetaKeyVersion(oldInfo.Bucket, oldInfo.Key, oldInfo.VersionID), oldHistData); err != nil {
					return err
				}
			}
		}

		data, err := json.Marshal(info)
		if err != nil {
			return err
		}
		if info.VersionID != "" {
			// Write historical version record.
			if err := b.Put(objectMetaKeyVersion(info.Bucket, info.Key, info.VersionID), data); err != nil {
				return err
			}
		} else {
			// Versioning is off for this write, so any history left behind by a
			// previously-versioned period is unreachable: the version records
			// could never be listed or deleted again and leaked forever.
			if err := deleteVersionRecords(b, info.Bucket, info.Key); err != nil {
				return err
			}
		}
		// Write/Update latest version pointer
		return b.Put(objectMetaKey(info.Bucket, info.Key), data)
	})
}

// deleteVersionRecords removes every historical version record for a key,
// leaving the latest-version pointer untouched.
func deleteVersionRecords(b *bolt.Bucket, bucket, key string) error {
	prefix := []byte(bucket + "\x00" + key + "\x00")
	c := b.Cursor()

	var stale [][]byte
	for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
		stale = append(stale, append([]byte(nil), k...))
	}
	for _, k := range stale {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// GetObjectMeta retrieves object metadata. If versionID is empty, retrieves the latest version.
func (m *MetadataStore) GetObjectMeta(bucket, key, versionID string) (*ObjectInfo, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return nil, err
	}
	defer releasedb()
	var info ObjectInfo
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		var data []byte
		if versionID != "" {
			data = b.Get(objectMetaKeyVersion(bucket, key, versionID))
		} else {
			data = b.Get(objectMetaKey(bucket, key))
		}
		// Objects written before versioning was enabled are the S3 null
		// version, even before the next write promotes their metadata.
		if data == nil && versionID == "null" {
			legacy := b.Get(objectMetaKey(bucket, key))
			if legacy != nil {
				var candidate ObjectInfo
				if err := json.Unmarshal(legacy, &candidate); err != nil {
					return err
				}
				if candidate.VersionID == "" {
					data = legacy
				}
			}
		}
		if data == nil {
			return fmt.Errorf("%w: %s/%s (version=%s)", errObjectMetaNotFound, bucket, key, versionID)
		}
		return json.Unmarshal(data, &info)
	})
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// DeleteObjectMeta removes object metadata. If versionID is specified, deletes only that version.
// If latest is deleted, updates the latest pointer to the next newest version in history.
func (m *MetadataStore) DeleteObjectMeta(bucket, key, versionID string) error {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
		if err := m.markReplication(tx, key); err != nil {
			return err
		}
		b := tx.Bucket(objectsBucket)
		if versionID != "" {
			// Delete specific version
			err = b.Delete(objectMetaKeyVersion(bucket, key, versionID))
			if err != nil {
				return err
			}

			// If this was the latest pointer, we must find the next newest version
			latestData := b.Get(objectMetaKey(bucket, key))
			if latestData != nil {
				var latestInfo ObjectInfo
				if err := json.Unmarshal(latestData, &latestInfo); err != nil {
					return err
				}
				if latestInfo.VersionID == versionID {
					// We just deleted the latest version! Let's find the next newest version from history
					nextLatest, err := m.findNextNewestVersion(tx, bucket, key, versionID)
					if err != nil {
						return err
					}
					if nextLatest != nil {
						nextLatest.IsLatest = true
						nextData, err := json.Marshal(nextLatest)
						if err != nil {
							return err
						}
						if err := b.Put(objectMetaKey(bucket, key), nextData); err != nil {
							return err
						}
						if nextLatest.VersionID != "" {
							if err := b.Put(objectMetaKeyVersion(bucket, key, nextLatest.VersionID), nextData); err != nil {
								return err
							}
						}
					} else {
						if err := b.Delete(objectMetaKey(bucket, key)); err != nil {
							return err
						}
						if err := adjustObjectCount(tx, -1); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}

		// Delete all metadata (pointer and all historical versions)
		if b.Get(objectMetaKey(bucket, key)) != nil {
			if err := b.Delete(objectMetaKey(bucket, key)); err != nil {
				return err
			}
			if err := adjustObjectCount(tx, -1); err != nil {
				return err
			}
		}
		prefix := []byte(bucket + "\x00" + key + "\x00")
		c := b.Cursor()
		var keysToDelete [][]byte
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			keysToDelete = append(keysToDelete, keyCopy)
		}
		for _, k := range keysToDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *MetadataStore) findNextNewestVersion(tx *bolt.Tx, bucket, key, deletedVersionID string) (*ObjectInfo, error) {
	b := tx.Bucket(objectsBucket)
	prefix := []byte(bucket + "\x00" + key + "\x00")
	c := b.Cursor()

	var newest *ObjectInfo
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		var info ObjectInfo
		if err := json.Unmarshal(v, &info); err != nil {
			return nil, err
		}
		if info.VersionID != deletedVersionID {
			if newest == nil || info.LastModified.After(newest.LastModified) {
				newest = &info
			}
		}
	}
	return newest, nil
}
