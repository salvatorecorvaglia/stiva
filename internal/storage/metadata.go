package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketsBucket   = []byte("buckets")
	objectsBucket   = []byte("objects")
	multipartBucket = []byte("multipart")
)

type initLock struct {
	mu       sync.Mutex
	refCount int
}

// maxOpenBucketDBs bounds the number of cached per-bucket bbolt handles. It is
// a soft cap: a handle that is still in use is never evicted (see bucketDBEntry).
const maxOpenBucketDBs = 100

// bucketDBEntry is a refcounted bbolt handle.
//
// Refcounting exists because LRU eviction used to call Close() on a handle that
// other goroutines were still holding and about to run transactions against —
// long listings would fail with "database not open" once more than 100 buckets
// were in play. A handle is now only closed once its last user releases it.
type bucketDBEntry struct {
	db       *bolt.DB
	refCount int
	lastUsed time.Time
	// evicted marks a handle that lost its cache slot; the last release closes it.
	evicted bool
}

// MetadataStore manages bucket and object metadata using per-bucket bbolt databases.
type MetadataStore struct {
	globalDB      *bolt.DB
	activeBuckets map[string]*bucketDBEntry
	mu            sync.RWMutex
	bucketLocks   [32]*bucketLockSegment
	dataDir       string
	initLocks     map[string]*initLock
	initMu        sync.Mutex

	bucketCache map[string]*BucketInfo
	cacheMu     sync.RWMutex
	inMigration bool

	countCache map[string]countEntry
	countMu    sync.RWMutex
}

type bucketLockSegment struct {
	mu    sync.Mutex
	locks map[string]*bucketLock
}

type bucketLock struct {
	sync.RWMutex
	refCount int
}

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime = 16777619
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime
	}
	return hash
}

func (m *MetadataStore) getSegment(bucket string) *bucketLockSegment {
	idx := fnv32(bucket) % 32
	return m.bucketLocks[idx]
}

func (m *MetadataStore) acquireBucketLock(bucket string, write bool) func() {
	seg := m.getSegment(bucket)
	seg.mu.Lock()
	l, exists := seg.locks[bucket]
	if !exists {
		l = &bucketLock{}
		seg.locks[bucket] = l
	}
	l.refCount++
	seg.mu.Unlock()

	if write {
		l.Lock()
	} else {
		l.RLock()
	}

	return func() {
		if write {
			l.Unlock()
		} else {
			l.RUnlock()
		}

		seg.mu.Lock()
		l.refCount--
		if l.refCount == 0 {
			delete(seg.locks, bucket)
		}
		seg.mu.Unlock()
	}
}

// NewMetadataStore opens or creates the central database and sets up the metadata directory.
func NewMetadataStore(dataDir string) (*MetadataStore, error) {
	metaDir := filepath.Join(dataDir, "metadata")
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create metadata directory: %w", err)
	}

	globalPath := filepath.Join(dataDir, "stiva.db")
	globalDB, err := bolt.Open(globalPath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("failed to open central metadata db: %w", err)
	}

	err = globalDB.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketsBucket)
		return err
	})
	if err != nil {
		globalDB.Close()
		return nil, fmt.Errorf("failed to initialize central metadata db: %w", err)
	}

	m := &MetadataStore{
		globalDB:      globalDB,
		activeBuckets: make(map[string]*bucketDBEntry),
		dataDir:       dataDir,
		bucketCache:   make(map[string]*BucketInfo),
		countCache:    make(map[string]countEntry),
	}
	for i := 0; i < 32; i++ {
		m.bucketLocks[i] = &bucketLockSegment{
			locks: make(map[string]*bucketLock),
		}
	}

	if err := m.migrateIfNecessary(); err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("failed to run startup metadata migration: %w", err)
	}

	return m, nil
}

// Close closes all databases.
func (m *MetadataStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for bucket, entry := range m.activeBuckets {
		if err := entry.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close db for bucket %s: %w", bucket, err))
		}
	}
	m.activeBuckets = make(map[string]*bucketDBEntry)

	if err := m.globalDB.Close(); err != nil {
		errs = append(errs, fmt.Errorf("failed to close central metadata db: %w", err))
	}

	return errors.Join(errs...)
}

// releaseBucketDB drops one reference to a handle, closing it if it has been
// evicted from the cache and this was the last user.
func (m *MetadataStore) releaseBucketDB(entry *bucketDBEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry.refCount--
	if entry.refCount <= 0 && entry.evicted {
		_ = entry.db.Close()
	}
}

// evictLocked drops the least-recently-used idle handle to make room. Handles
// with live references are skipped rather than closed, so the cap is soft: it is
// better to briefly exceed it than to pull a database out from under a request.
// Callers must hold m.mu.
func (m *MetadataStore) evictLocked(keep string) {
	if len(m.activeBuckets) < maxOpenBucketDBs {
		return
	}

	var oldestBucket string
	var oldestTime time.Time
	for name, entry := range m.activeBuckets {
		if name == keep || entry.refCount > 0 {
			continue
		}
		if oldestBucket == "" || entry.lastUsed.Before(oldestTime) {
			oldestBucket = name
			oldestTime = entry.lastUsed
		}
	}
	if oldestBucket == "" {
		// Every cached handle is in use; keep them all open.
		return
	}

	entry := m.activeBuckets[oldestBucket]
	entry.evicted = true
	_ = entry.db.Close()
	delete(m.activeBuckets, oldestBucket)
}

// acquireBucketDB returns a bbolt handle for the bucket along with a release
// function that must be called when the caller is done with it.
func (m *MetadataStore) acquireBucketDB(bucket string) (*bolt.DB, func(), error) {
	m.mu.Lock()
	if entry, ok := m.activeBuckets[bucket]; ok {
		entry.lastUsed = time.Now()
		entry.refCount++
		m.mu.Unlock()
		return entry.db, func() { m.releaseBucketDB(entry) }, nil
	}
	m.mu.Unlock()

	// Verify bucket exists in global DB to prevent re-creation of deleted DB files (unless migrating)
	if !m.inMigration {
		exists := false
		err := m.globalDB.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketsBucket)
			if b != nil {
				exists = b.Get([]byte(bucket)) != nil
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		if !exists {
			return nil, nil, fmt.Errorf("bucket not found in registry: %s", bucket)
		}
	}

	// Get or create per-bucket initialization mutex
	m.initMu.Lock()
	if m.initLocks == nil {
		m.initLocks = make(map[string]*initLock)
	}
	lock, exists := m.initLocks[bucket]
	if !exists {
		lock = &initLock{}
		m.initLocks[bucket] = lock
	}
	lock.refCount++
	m.initMu.Unlock()

	lock.mu.Lock()
	defer func() {
		lock.mu.Unlock()
		m.initMu.Lock()
		lock.refCount--
		if lock.refCount == 0 {
			delete(m.initLocks, bucket)
		}
		m.initMu.Unlock()
	}()

	// Double check if already opened by another thread
	m.mu.Lock()
	if entry, ok := m.activeBuckets[bucket]; ok {
		entry.lastUsed = time.Now()
		entry.refCount++
		m.mu.Unlock()
		return entry.db, func() { m.releaseBucketDB(entry) }, nil
	}
	m.mu.Unlock()

	// Open the database file (blocking Disk I/O) without holding global locks!
	dbPath := filepath.Join(m.dataDir, "metadata", bucket+".db")
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open metadata db for bucket %s: %w", bucket, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(objectsBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(multipartBucket); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("failed to initialize metadata db for bucket %s: %w", bucket, err)
	}

	m.mu.Lock()
	// Another goroutine may have won the race while we were opening.
	if existing, ok := m.activeBuckets[bucket]; ok {
		m.mu.Unlock()
		_ = db.Close()
		m.mu.Lock()
		existing.lastUsed = time.Now()
		existing.refCount++
		m.mu.Unlock()
		return existing.db, func() { m.releaseBucketDB(existing) }, nil
	}

	m.evictLocked(bucket)

	entry := &bucketDBEntry{db: db, refCount: 1, lastUsed: time.Now()}
	m.activeBuckets[bucket] = entry
	m.mu.Unlock()

	return db, func() { m.releaseBucketDB(entry) }, nil
}

func (m *MetadataStore) closeAndRemoveBucketDB(bucket string) error {
	m.mu.Lock()
	if entry, ok := m.activeBuckets[bucket]; ok {
		delete(m.activeBuckets, bucket)
		if entry.refCount > 0 {
			// Someone is mid-transaction; mark it so the last release closes it.
			entry.evicted = true
		} else {
			_ = entry.db.Close()
		}
	}
	m.mu.Unlock()

	dbPath := filepath.Join(m.dataDir, "metadata", bucket+".db")
	return os.Remove(dbPath)
}

func (m *MetadataStore) migrateIfNecessary() error {
	var objectsBucketExists, multipartBucketExists bool
	err := m.globalDB.View(func(tx *bolt.Tx) error {
		objectsBucketExists = tx.Bucket(objectsBucket) != nil
		multipartBucketExists = tx.Bucket(multipartBucket) != nil
		return nil
	})
	if err != nil {
		return err
	}

	if !objectsBucketExists && !multipartBucketExists {
		return nil
	}

	m.inMigration = true
	defer func() { m.inMigration = false }()

	slog.Info("[Migration] Migrating old single-database metadata to per-bucket databases")

	if objectsBucketExists {
		var errs []error
		err = m.globalDB.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(objectsBucket)
			return b.ForEach(func(k, v []byte) error {
				parts := bytes.SplitN(k, []byte("\x00"), 2)
				if len(parts) != 2 {
					return nil
				}
				bucketName := string(parts[0])

				bucketDB, releasebucketDB, err := m.acquireBucketDB(bucketName)
				if err != nil {
					errs = append(errs, err)
					return nil
				}
				defer releasebucketDB()

				err = bucketDB.Update(func(btx *bolt.Tx) error {
					bb, err := btx.CreateBucketIfNotExists(objectsBucket)
					if err != nil {
						return err
					}
					return bb.Put(k, v)
				})
				if err != nil {
					errs = append(errs, err)
				}
				return nil
			})
		})
		if err != nil {
			return err
		}
		if len(errs) > 0 {
			return errs[0]
		}
	}

	if multipartBucketExists {
		var errs []error
		err = m.globalDB.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(multipartBucket)
			return b.ForEach(func(k, v []byte) error {
				parts := bytes.SplitN(k, []byte("\x00"), 3)
				if len(parts) < 1 {
					return nil
				}
				bucketName := string(parts[0])

				bucketDB, releasebucketDB, err := m.acquireBucketDB(bucketName)
				if err != nil {
					errs = append(errs, err)
					return nil
				}
				defer releasebucketDB()

				err = bucketDB.Update(func(btx *bolt.Tx) error {
					bb, err := btx.CreateBucketIfNotExists(multipartBucket)
					if err != nil {
						return err
					}
					return bb.Put(k, v)
				})
				if err != nil {
					errs = append(errs, err)
				}
				return nil
			})
		})
		if err != nil {
			return err
		}
		if len(errs) > 0 {
			return errs[0]
		}
	}

	err = m.globalDB.Update(func(tx *bolt.Tx) error {
		if objectsBucketExists {
			_ = tx.DeleteBucket(objectsBucket)
		}
		if multipartBucketExists {
			_ = tx.DeleteBucket(multipartBucket)
		}
		return nil
	})
	if err != nil {
		return err
	}

	slog.Info("[Migration] Migration completed successfully")
	return nil
}

// PutBucket stores bucket metadata.
func (m *MetadataStore) PutBucket(info *BucketInfo) error {
	unlock := m.acquireBucketLock(info.Name, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(info.Name), data)
	})
	if err == nil {
		m.cacheMu.Lock()
		m.bucketCache[info.Name] = info
		m.cacheMu.Unlock()
	}
	return err
}

// GetBucket retrieves bucket metadata by name.
func (m *MetadataStore) GetBucket(name string) (*BucketInfo, error) {
	unlock := m.acquireBucketLock(name, false)
	defer unlock()
	var info BucketInfo
	err := m.globalDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", name)
		}
		return json.Unmarshal(data, &info)
	})
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// maxBucketCacheEntries bounds the bucket metadata cache. It previously grew
// without limit, and cached a nil entry for every name that was looked up and
// not found — so probing random bucket names grew the map indefinitely.
const maxBucketCacheEntries = 1024

// GetBucketCached retrieves bucket metadata by name from the cache.
func (m *MetadataStore) GetBucketCached(name string) (*BucketInfo, error) {
	m.cacheMu.RLock()
	info, exists := m.bucketCache[name]
	m.cacheMu.RUnlock()

	if exists {
		if info == nil {
			return nil, fmt.Errorf("bucket not found: %s", name)
		}
		return info, nil
	}

	info, err := m.GetBucket(name)

	m.cacheMu.Lock()
	// Drop the whole cache rather than track per-entry recency: entries are
	// cheap to rebuild and this bound is only a safety valve.
	if len(m.bucketCache) >= maxBucketCacheEntries {
		m.bucketCache = make(map[string]*BucketInfo, maxBucketCacheEntries)
	}
	if err != nil {
		m.bucketCache[name] = nil
	} else {
		m.bucketCache[name] = info
	}
	m.cacheMu.Unlock()

	return info, err
}

// DeleteBucket removes bucket metadata and deletes its DB file.
func (m *MetadataStore) DeleteBucket(name string) error {
	unlock := m.acquireBucketLock(name, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		return b.Delete([]byte(name))
	})
	if err != nil {
		return err
	}
	m.cacheMu.Lock()
	delete(m.bucketCache, name)
	m.cacheMu.Unlock()
	m.invalidateCount(name)
	return m.closeAndRemoveBucketDB(name)
}

// IsBucketEmpty checks if both objects and multipart uploads are empty.
func (m *MetadataStore) IsBucketEmpty(bucket string) (bool, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()

	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return false, err
	}
	defer releasedb()

	// Any record at all means the bucket is not empty: like S3, every object
	// version and delete marker must be removed before the bucket can go.
	//
	// This walks a cursor and stops at the first key rather than calling
	// Stats(), which traverses every page of the bucket just to produce a count
	// that is then compared against zero.
	isEmpty := true
	err = db.View(func(tx *bolt.Tx) error {
		if objB := tx.Bucket(objectsBucket); objB != nil {
			if k, _ := objB.Cursor().First(); k != nil {
				isEmpty = false
				return nil
			}
		}
		if mpB := tx.Bucket(multipartBucket); mpB != nil {
			if k, _ := mpB.Cursor().First(); k != nil {
				isEmpty = false
			}
		}
		return nil
	})
	return isEmpty, err
}

// ListBuckets returns all stored bucket metadata.
func (m *MetadataStore) ListBuckets() ([]BucketInfo, error) {
	var buckets []BucketInfo
	err := m.globalDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		return b.ForEach(func(k, v []byte) error {
			if bytes.HasPrefix(k, []byte("_sys_")) {
				return nil
			}
			var info BucketInfo
			if err := json.Unmarshal(v, &info); err != nil {
				return err
			}
			buckets = append(buckets, info)
			return nil
		})
	})
	return buckets, err
}

// BucketExists checks if a bucket exists in the central metadata store.
func (m *MetadataStore) BucketExists(name string) (bool, error) {
	unlock := m.acquireBucketLock(name, false)
	defer unlock()
	exists := false
	err := m.globalDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		exists = b.Get([]byte(name)) != nil
		return nil
	})
	return exists, err
}

// PutBucketCORS sets CORS configuration for a bucket.
func (m *MetadataStore) PutBucketCORS(bucket string, cors *CORSConfiguration) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.CORS = cors
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.cacheMu.Lock()
		delete(m.bucketCache, bucket)
		m.cacheMu.Unlock()
	}
	return err
}

// GetBucketCORS gets CORS configuration for a bucket.
func (m *MetadataStore) GetBucketCORS(bucket string) (*CORSConfiguration, error) {
	info, err := m.GetBucketCached(bucket)
	if err != nil {
		return nil, err
	}
	return info.CORS, nil
}

// DeleteBucketCORS deletes CORS configuration for a bucket.
func (m *MetadataStore) DeleteBucketCORS(bucket string) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.CORS = nil
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.cacheMu.Lock()
		delete(m.bucketCache, bucket)
		m.cacheMu.Unlock()
	}
	return err
}

// PutBucketVersioning sets versioning configuration for a bucket.
func (m *MetadataStore) PutBucketVersioning(bucket string, status string) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.Versioning = status
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.cacheMu.Lock()
		delete(m.bucketCache, bucket)
		m.cacheMu.Unlock()
	}
	return err
}

// GetBucketVersioning gets versioning configuration for a bucket.
func (m *MetadataStore) GetBucketVersioning(bucket string) (string, error) {
	info, err := m.GetBucketCached(bucket)
	if err != nil {
		return "", err
	}
	return info.Versioning, nil
}

// SetBucketPublic sets the public status of a bucket.
func (m *MetadataStore) SetBucketPublic(bucket string, public bool) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.IsPublic = public
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.cacheMu.Lock()
		delete(m.bucketCache, bucket)
		m.cacheMu.Unlock()
	}
	return err
}

// IsBucketPublic gets the public status of a bucket.
func (m *MetadataStore) IsBucketPublic(bucket string) (bool, error) {
	info, err := m.GetBucketCached(bucket)
	if err != nil {
		return false, err
	}
	return info.IsPublic, nil
}

func objectMetaKey(bucket, key string) []byte {
	return []byte(bucket + "\x00" + key)
}

func objectMetaKeyVersion(bucket, key, versionID string) []byte {
	return []byte(bucket + "\x00" + key + "\x00" + versionID)
}

// PutObjectMeta stores object metadata (both the latest pointer and the historical record).
func (m *MetadataStore) PutObjectMeta(info *ObjectInfo) error {
	defer m.invalidateCount(info.Bucket)
	unlock := m.acquireBucketLock(info.Bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(info.Bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)

		info.IsLatest = true

		// Update old latest version if present
		oldData := b.Get(objectMetaKey(info.Bucket, info.Key))
		if oldData != nil {
			var oldInfo ObjectInfo
			if err := json.Unmarshal(oldData, &oldInfo); err == nil {
				if oldInfo.VersionID != "" && oldInfo.VersionID != info.VersionID {
					oldInfo.IsLatest = false
					oldHistData, _ := json.Marshal(oldInfo)
					_ = b.Put(objectMetaKeyVersion(oldInfo.Bucket, oldInfo.Key, oldInfo.VersionID), oldHistData)
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
		if data == nil {
			return fmt.Errorf("object not found: %s/%s (version=%s)", bucket, key, versionID)
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
	defer m.invalidateCount(bucket)
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
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
				if err := json.Unmarshal(latestData, &latestInfo); err == nil && latestInfo.VersionID == versionID {
					// We just deleted the latest version! Let's find the next newest version from history
					nextLatest, err := m.findNextNewestVersion(tx, bucket, key, versionID)
					if err == nil && nextLatest != nil {
						nextLatest.IsLatest = true
						nextData, _ := json.Marshal(nextLatest)
						_ = b.Put(objectMetaKey(bucket, key), nextData)
						if nextLatest.VersionID != "" {
							_ = b.Put(objectMetaKeyVersion(bucket, key, nextLatest.VersionID), nextData)
						}
					} else {
						_ = b.Delete(objectMetaKey(bucket, key))
					}
				}
			}
			return nil
		}

		// Delete all metadata (pointer and all historical versions)
		_ = b.Delete(objectMetaKey(bucket, key))
		prefix := []byte(bucket + "\x00" + key + "\x00")
		c := b.Cursor()
		var keysToDelete [][]byte
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			keysToDelete = append(keysToDelete, keyCopy)
		}
		for _, k := range keysToDelete {
			_ = b.Delete(k)
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
		if err := json.Unmarshal(v, &info); err == nil {
			if info.VersionID != deletedVersionID {
				if newest == nil || info.LastModified.After(newest.LastModified) {
					newest = &info
				}
			}
		}
	}
	return newest, nil
}

// ListAllObjectMetas returns all object metadata for a given bucket (latest versions only).
func (m *MetadataStore) ListAllObjectMetas(bucket string) ([]ObjectInfo, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return nil, err
	}
	defer releasedb()
	var objects []ObjectInfo
	prefix := []byte(bucket + "\x00")
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			// Skip version history records (which contain another \x00)
			objectKey := string(k[len(prefix):])
			if strings.Contains(objectKey, "\x00") {
				continue
			}

			var info ObjectInfo
			if err := json.Unmarshal(v, &info); err != nil {
				return err
			}
			objects = append(objects, info)
		}
		return nil
	})
	return objects, err
}

// countCacheTTL bounds how long a cached object count may be reused when no
// write has invalidated it. Writes invalidate eagerly, so this only limits
// staleness from changes made outside this process.
const countCacheTTL = 30 * time.Second

type countEntry struct {
	count      int
	computedAt time.Time
}

// invalidateCount drops the cached object count for a bucket. Called from every
// path that adds or removes a latest-version pointer.
func (m *MetadataStore) invalidateCount(bucket string) {
	m.countMu.Lock()
	delete(m.countCache, bucket)
	m.countMu.Unlock()
}

// CountObjectMetas counts latest objects in a bucket.
//
// The result is memoised: the console lists every bucket on each dashboard load
// and refreshes every ten seconds, so an uncached count meant a full cursor scan
// of every bucket's database on every page view.
func (m *MetadataStore) CountObjectMetas(bucket string) (int, error) {
	m.countMu.RLock()
	entry, ok := m.countCache[bucket]
	m.countMu.RUnlock()
	if ok && time.Since(entry.computedAt) < countCacheTTL {
		return entry.count, nil
	}

	count, err := m.countObjectMetasUncached(bucket)
	if err != nil {
		return 0, err
	}

	m.countMu.Lock()
	m.countCache[bucket] = countEntry{count: count, computedAt: time.Now()}
	m.countMu.Unlock()

	return count, nil
}

func (m *MetadataStore) countObjectMetasUncached(bucket string) (int, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return 0, err
	}
	defer releasedb()
	count := 0
	prefix := []byte(bucket + "\x00")
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			objectKey := string(k[len(prefix):])
			if strings.Contains(objectKey, "\x00") {
				continue
			}
			count++
		}
		return nil
	})
	return count, err
}

// DeleteAllObjectMetas removes all object metadata for a given bucket.
func (m *MetadataStore) DeleteAllObjectMetas(bucket string) error {
	defer m.invalidateCount(bucket)
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		c := b.Cursor()
		var keysToDelete [][]byte
		prefix := []byte(bucket + "\x00")
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			keysToDelete = append(keysToDelete, keyCopy)
		}
		for _, key := range keysToDelete {
			if err := b.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
}

// multipartKey returns the composite key for a multipart upload.
func multipartKey(bucket, key, uploadID string) []byte {
	return []byte(bucket + "\x00" + key + "\x00" + uploadID)
}

// MultipartMeta stores multipart upload metadata.
//
// The customer-provided SSE-C key is deliberately NOT a field here. It used to
// be persisted verbatim into bbolt, which defeated the entire point of SSE-C:
// anyone with read access to the metadata database could decrypt every
// multipart object. Only the key's MD5 is retained, purely so that subsequent
// UploadPart and CompleteMultipartUpload calls can verify the caller supplied
// the same key. Callers must now present the key on CompleteMultipartUpload.
type MultipartMeta struct {
	UploadID             string     `json:"uploadId"`
	Bucket               string     `json:"bucket"`
	Key                  string     `json:"key"`
	ContentType          string     `json:"contentType"`
	Created              time.Time  `json:"created"`
	Parts                []PartInfo `json:"parts"`
	SSECustomerAlgorithm string     `json:"sseCustomerAlgorithm,omitempty"`
	SSECustomerKeyMD5    string     `json:"sseCustomerKeyMD5,omitempty"`
}

// PutMultipartMeta stores multipart upload metadata.
func (m *MetadataStore) PutMultipartMeta(meta *MultipartMeta) error {
	unlock := m.acquireBucketLock(meta.Bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(meta.Bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(multipartBucket)
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return b.Put(multipartKey(meta.Bucket, meta.Key, meta.UploadID), data)
	})
}

// GetMultipartMeta retrieves multipart upload metadata.
func (m *MetadataStore) GetMultipartMeta(bucket, key, uploadID string) (*MultipartMeta, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return nil, err
	}
	defer releasedb()
	var meta MultipartMeta
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(multipartBucket)
		data := b.Get(multipartKey(bucket, key, uploadID))
		if data == nil {
			return fmt.Errorf("multipart upload not found: %s/%s/%s", bucket, key, uploadID)
		}
		return json.Unmarshal(data, &meta)
	})
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// DeleteMultipartMeta removes multipart upload metadata.
func (m *MetadataStore) DeleteMultipartMeta(bucket, key, uploadID string) error {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(multipartBucket)
		return b.Delete(multipartKey(bucket, key, uploadID))
	})
}

// IterateMultipartMetas streams every in-progress multipart upload for a bucket
// to the callback, in key order.
func (m *MetadataStore) IterateMultipartMetas(bucket string, fn func(meta *MultipartMeta) error) error {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(multipartBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var meta MultipartMeta
			if err := json.Unmarshal(v, &meta); err != nil {
				return err
			}
			return fn(&meta)
		})
	})
}

// PutBucketLifecycle sets lifecycle configuration for a bucket.
func (m *MetadataStore) PutBucketLifecycle(bucket string, lc *LifecycleConfiguration) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.Lifecycle = lc
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.cacheMu.Lock()
		delete(m.bucketCache, bucket)
		m.cacheMu.Unlock()
	}
	return err
}

// GetBucketLifecycle gets lifecycle configuration for a bucket.
func (m *MetadataStore) GetBucketLifecycle(bucket string) (*LifecycleConfiguration, error) {
	info, err := m.GetBucketCached(bucket)
	if err != nil {
		return nil, err
	}
	return info.Lifecycle, nil
}

// DeleteBucketLifecycle deletes lifecycle configuration for a bucket.
func (m *MetadataStore) DeleteBucketLifecycle(bucket string) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.Lifecycle = nil
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.cacheMu.Lock()
		delete(m.bucketCache, bucket)
		m.cacheMu.Unlock()
	}
	return err
}

// PutBucketLogging sets logging configuration for a bucket.
func (m *MetadataStore) PutBucketLogging(bucket string, logging *BucketLoggingStatus) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket not found: %s", bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.Logging = logging
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.cacheMu.Lock()
		delete(m.bucketCache, bucket)
		m.cacheMu.Unlock()
	}
	return err
}

// GetBucketLogging gets logging configuration for a bucket.
func (m *MetadataStore) GetBucketLogging(bucket string) (*BucketLoggingStatus, error) {
	info, err := m.GetBucketCached(bucket)
	if err != nil {
		return nil, err
	}
	return info.Logging, nil
}

// PutObjectMetaRaw stores object metadata exactly as provided, without overriding fields.
func (m *MetadataStore) PutObjectMetaRaw(info *ObjectInfo) error {
	defer m.invalidateCount(info.Bucket)
	unlock := m.acquireBucketLock(info.Bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(info.Bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		data, err := json.Marshal(info)
		if err != nil {
			return err
		}
		if info.VersionID != "" {
			err = b.Put(objectMetaKeyVersion(info.Bucket, info.Key, info.VersionID), data)
			if err != nil {
				return err
			}
		}
		if info.IsLatest {
			return b.Put(objectMetaKey(info.Bucket, info.Key), data)
		}
		return nil
	})
}

// GetSystemValue retrieves a system configuration value from the central database.
func (m *MetadataStore) GetSystemValue(key string) (string, error) {
	var val string
	err := m.globalDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte("_sys_" + key))
		if data != nil {
			val = string(data)
		}
		return nil
	})
	return val, err
}

// PutSystemValue stores a system configuration value in the central database.
func (m *MetadataStore) PutSystemValue(key, val string) error {
	return m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		return b.Put([]byte("_sys_"+key), []byte(val))
	})
}

// GetObjectVersions returns all version records (including latest and historical) for a specific key.
func (m *MetadataStore) GetObjectVersions(bucket, key string) ([]ObjectInfo, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return nil, err
	}
	defer releasedb()
	var versions []ObjectInfo
	prefix := []byte(bucket + "\x00" + key + "\x00")
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return nil
		}
		// Also read the latest version pointer
		latestData := b.Get(objectMetaKey(bucket, key))
		if latestData != nil {
			var latestInfo ObjectInfo
			if err := json.Unmarshal(latestData, &latestInfo); err == nil {
				versions = append(versions, latestInfo)
			}
		}

		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var info ObjectInfo
			if err := json.Unmarshal(v, &info); err == nil {
				// Avoid double adding if it's already there (though historical keys are distinct)
				alreadyAdded := false
				for _, existing := range versions {
					if existing.VersionID == info.VersionID {
						alreadyAdded = true
						break
					}
				}
				if !alreadyAdded {
					versions = append(versions, info)
				}
			}
		}
		return nil
	})
	return versions, err
}

// IterateObjectMetas iterates through all object metadata for a given bucket (latest versions only) streaming it to a callback.
func (m *MetadataStore) IterateObjectMetas(bucket string, fn func(info *ObjectInfo) error) error {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	prefix := []byte(bucket + "\x00")
	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			objectKey := string(k[len(prefix):])
			if strings.Contains(objectKey, "\x00") {
				continue
			}

			var info ObjectInfo
			if err := json.Unmarshal(v, &info); err != nil {
				return err
			}
			if err := fn(&info); err != nil {
				return err
			}
		}
		return nil
	})
}

// IterateObjectVersions iterates through all object version records (excluding latest pointers) streaming to a callback.
func (m *MetadataStore) IterateObjectVersions(bucket string, fn func(info *ObjectInfo) error) error {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return err
	}
	defer releasedb()
	prefix := []byte(bucket + "\x00")
	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			objectKey := string(k[len(prefix):])
			if !strings.Contains(objectKey, "\x00") {
				continue
			}

			var info ObjectInfo
			if err := json.Unmarshal(v, &info); err != nil {
				return err
			}
			if err := fn(&info); err != nil {
				return err
			}
		}
		return nil
	})
}
