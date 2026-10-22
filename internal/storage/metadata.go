package storage

import (
	"bytes"
	"encoding/binary"
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
	// statsBucket holds per-bucket counters kept in step with the records
	// they summarize; objectCountKey is the number of current-version records.
	statsBucket    = []byte("stats")
	objectCountKey = []byte("objects")
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
	replication bool

	globalDB      *bolt.DB
	activeBuckets map[string]*bucketDBEntry
	mu            sync.RWMutex
	bucketLocks   [32]*bucketLockSegment
	dataDir       string
	initLocks     map[string]*initLock
	initMu        sync.Mutex

	bucketCache map[string]*BucketInfo
	cacheMu     sync.RWMutex
	// cacheGen is bumped (under cacheMu) by every bucket-config write. A cache
	// fill records it before reading the database and only stores its result
	// if it is unchanged, so a read that raced a write can't overwrite the
	// write's invalidation with the old value.
	cacheGen    uint64
	inMigration bool
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

	if err := syncParents(metaDir, dataDir); err != nil {
		_ = m.Close()
		return nil, err
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
		return initObjectCount(tx)
	})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("failed to initialize metadata db for bucket %s: %w", bucket, err)
	}

	if err := syncParents(filepath.Dir(dbPath), m.dataDir); err != nil {
		_ = db.Close()
		return nil, nil, err
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
		if err := m.migrateRecords(objectsBucket); err != nil {
			return err
		}
	}
	if multipartBucketExists {
		if err := m.migrateRecords(multipartBucket); err != nil {
			return err
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

// migrationBatchSize is how many records migration writes per transaction.
const migrationBatchSize = 1000

// migrateRecords copies every record of one top-level bbolt bucket in the old
// single-file database into the per-bucket databases.
//
// It writes in batches. It used to commit one transaction — one fsync — per
// record, so the first start after upgrading a large installation could take
// hours. Keys begin with the bucket name, so each bucket's records are
// contiguous and a batch never spans two databases.
func (m *MetadataStore) migrateRecords(src []byte) error {
	type record struct{ k, v []byte }
	var batch []record
	batchBucket := ""

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		db, release, err := m.acquireBucketDB(batchBucket)
		if err != nil {
			return err
		}
		defer release()
		err = db.Update(func(tx *bolt.Tx) error {
			dst, err := tx.CreateBucketIfNotExists(src)
			if err != nil {
				return err
			}
			for _, r := range batch {
				if err := dst.Put(r.k, r.v); err != nil {
					return err
				}
			}
			// These records bypass the writes that maintain the object
			// count, so drop it and let the first read recount.
			if sb := tx.Bucket(statsBucket); sb != nil {
				return sb.Delete(objectCountKey)
			}
			return nil
		})
		batch = batch[:0]
		return err
	}

	err := m.globalDB.View(func(tx *bolt.Tx) error {
		return tx.Bucket(src).ForEach(func(k, v []byte) error {
			name, _, found := bytes.Cut(k, []byte{0})
			if !found && bytes.Equal(src, objectsBucket) {
				return nil // not an object record
			}
			if len(batch) > 0 && (string(name) != batchBucket || len(batch) >= migrationBatchSize) {
				if err := flush(); err != nil {
					return err
				}
			}
			batchBucket = string(name)
			// bbolt's keys and values are only valid inside the transaction.
			batch = append(batch, record{append([]byte(nil), k...), append([]byte(nil), v...)})
			return nil
		})
	})
	if err != nil {
		return err
	}
	return flush()
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
		m.cacheGen++
		m.bucketCache[info.Name] = info
		m.cacheMu.Unlock()
	}
	return err
}

// errNoSuchBucketRecord marks a lookup of a bucket that is not in the
// registry, as opposed to a failure to read the registry at all.
var errNoSuchBucketRecord = errors.New("bucket not found")

// GetBucket retrieves bucket metadata by name.
func (m *MetadataStore) GetBucket(name string) (*BucketInfo, error) {
	unlock := m.acquireBucketLock(name, false)
	defer unlock()
	var info BucketInfo
	err := m.globalDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("%w: %s", errNoSuchBucketRecord, name)
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
			return nil, fmt.Errorf("%w: %s", errNoSuchBucketRecord, name)
		}
		return info, nil
	}

	gen := m.bucketCacheGeneration()
	info, err := m.GetBucket(name)
	m.fillBucketCache(name, info, err, gen)

	return info, err
}

// bucketCacheGeneration returns the current cache generation; pass it to
// fillBucketCache after reading from the database.
func (m *MetadataStore) bucketCacheGeneration() uint64 {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return m.cacheGen
}

// fillBucketCache stores a database read in the cache, unless a bucket-config
// write happened since gen was taken. Such a write may have committed before
// or after the read; either way the read may be stale, and keeping it would
// serve the old config (a bucket made private still read as public) with no
// expiry to correct it.
func (m *MetadataStore) fillBucketCache(name string, info *BucketInfo, err error, gen uint64) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.cacheGen != gen {
		return
	}
	// Drop the whole cache rather than track per-entry recency: entries are
	// cheap to rebuild and this bound is only a safety valve.
	if len(m.bucketCache) >= maxBucketCacheEntries {
		m.bucketCache = make(map[string]*BucketInfo, maxBucketCacheEntries)
	}
	switch {
	case err == nil:
		m.bucketCache[name] = info
	case errors.Is(err, errNoSuchBucketRecord):
		m.bucketCache[name] = nil
	default:
		// A failed read says nothing about the bucket; caching it as
		// missing would hide the bucket until its next config change.
	}
}

// invalidateBucketCache drops a bucket's cached config after a write to it.
func (m *MetadataStore) invalidateBucketCache(name string) {
	m.cacheMu.Lock()
	m.cacheGen++
	delete(m.bucketCache, name)
	m.cacheMu.Unlock()
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
	m.invalidateBucketCache(name)
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
		if pending := tx.Bucket(replicationBucket); pending != nil {
			if k, _ := pending.Cursor().First(); k != nil {
				isEmpty = false
				return nil
			}
		}
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
//
// It is answered from the bucket cache, whose generation guard keeps it
// consistent with bucket creation and deletion. It used to open a database
// transaction on every call — and nearly every engine operation, plus the
// router's per-request logging and public-access checks, makes one.
func (m *MetadataStore) BucketExists(name string) (bool, error) {
	_, err := m.GetBucketCached(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errNoSuchBucketRecord):
		return false, nil
	default:
		return false, err
	}
}

// updateBucketInfo applies mutate to a bucket's stored configuration in one
// transaction, under the bucket's write lock, and invalidates its cached copy.
func (m *MetadataStore) updateBucketInfo(bucket string, mutate func(*BucketInfo)) error {
	unlock := m.acquireBucketLock(bucket, true)
	defer unlock()
	err := m.globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("%w: %s", errNoSuchBucketRecord, bucket)
		}
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		mutate(&info)
		newData, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(bucket), newData)
	})
	if err == nil {
		m.invalidateBucketCache(bucket)
	}
	return err
}

// PutBucketCORS sets CORS configuration for a bucket.
func (m *MetadataStore) PutBucketCORS(bucket string, cors *CORSConfiguration) error {
	return m.updateBucketInfo(bucket, func(info *BucketInfo) {
		info.CORS = cors
	})
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
	return m.updateBucketInfo(bucket, func(info *BucketInfo) {
		info.CORS = nil
	})
}

// PutBucketVersioning sets versioning configuration for a bucket.
func (m *MetadataStore) PutBucketVersioning(bucket string, status string) error {
	return m.updateBucketInfo(bucket, func(info *BucketInfo) {
		info.Versioning = status
	})
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
	return m.updateBucketInfo(bucket, func(info *BucketInfo) {
		info.IsPublic = public
	})
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
			if err := json.Unmarshal(oldData, &oldInfo); err == nil {
				if oldInfo.VersionID != "" && oldInfo.VersionID != info.VersionID {
					oldInfo.IsLatest = false
					oldHistData, err := json.Marshal(oldInfo)
					if err != nil {
						return err
					}
					if err := b.Put(objectMetaKeyVersion(oldInfo.Bucket, oldInfo.Key, oldInfo.VersionID), oldHistData); err != nil {
						return err
					}
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
				if err := json.Unmarshal(latestData, &latestInfo); err == nil && latestInfo.VersionID == versionID {
					// We just deleted the latest version! Let's find the next newest version from history
					nextLatest, err := m.findNextNewestVersion(tx, bucket, key, versionID)
					if err == nil && nextLatest != nil {
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

// initObjectCount records a zero object count for a bucket database that has
// no count and no objects yet (a new bucket). A database that already holds
// objects but no count predates stored counts, and is left for
// CountObjectMetas to count once.
func initObjectCount(tx *bolt.Tx) error {
	sb, err := tx.CreateBucketIfNotExists(statsBucket)
	if err != nil {
		return err
	}
	if sb.Get(objectCountKey) != nil {
		return nil
	}
	if ob := tx.Bucket(objectsBucket); ob != nil {
		if k, _ := ob.Cursor().First(); k != nil {
			return nil
		}
	}
	return setObjectCount(tx, 0)
}

func setObjectCount(tx *bolt.Tx, n uint64) error {
	sb, err := tx.CreateBucketIfNotExists(statsBucket)
	if err != nil {
		return err
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], n)
	return sb.Put(objectCountKey, buf[:])
}

// adjustObjectCount applies delta to the stored object count, within the
// same transaction as the change it accounts for. With no stored count yet
// it does nothing: the first read computes one. A count that would go
// negative has drifted from the records, so it is dropped to be recomputed.
func adjustObjectCount(tx *bolt.Tx, delta int64) error {
	sb := tx.Bucket(statsBucket)
	if sb == nil {
		return nil
	}
	v := sb.Get(objectCountKey)
	if v == nil {
		return nil
	}
	n := int64(binary.BigEndian.Uint64(v)) + delta
	if n < 0 {
		return sb.Delete(objectCountKey)
	}
	return setObjectCount(tx, uint64(n))
}

// scanObjectCount counts current-version records (keys with no version
// suffix) by walking the whole bucket.
func scanObjectCount(tx *bolt.Tx, bucket string) int {
	count := 0
	prefix := []byte(bucket + "\x00")
	c := tx.Bucket(objectsBucket).Cursor()
	for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
		if !bytes.Contains(k[len(prefix):], []byte{0}) {
			count++
		}
	}
	return count
}

// CountObjectMetas returns the number of current-version records (latest
// objects and delete markers) in a bucket.
//
// It reads a count kept up to date in the same transactions that add or
// remove those records. It used to scan the whole bucket, memoised only until
// the next write — so on a busy bucket nearly every console dashboard load,
// which counts every bucket, did a full scan. A database without a stored
// count gets one from a single scan, made in a write transaction so no
// concurrent change can slip between the scan and the store.
func (m *MetadataStore) CountObjectMetas(bucket string) (int, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return 0, err
	}
	defer releasedb()

	count := -1
	err = db.View(func(tx *bolt.Tx) error {
		if sb := tx.Bucket(statsBucket); sb != nil {
			if v := sb.Get(objectCountKey); v != nil {
				count = int(binary.BigEndian.Uint64(v))
			}
		}
		return nil
	})
	if err != nil || count >= 0 {
		return count, err
	}

	err = db.Update(func(tx *bolt.Tx) error {
		count = scanObjectCount(tx, bucket)
		return setObjectCount(tx, uint64(count))
	})
	return count, err
}

// countObjectMetasByScan counts by walking the bucket, ignoring the stored
// count; tests use it as the reference.
func (m *MetadataStore) countObjectMetasByScan(bucket string) (int, error) {
	unlock := m.acquireBucketLock(bucket, false)
	defer unlock()
	db, releasedb, err := m.acquireBucketDB(bucket)
	if err != nil {
		return 0, err
	}
	defer releasedb()
	count := 0
	err = db.View(func(tx *bolt.Tx) error {
		count = scanObjectCount(tx, bucket)
		return nil
	})
	return count, err
}

// DeleteAllObjectMetas removes all object metadata for a given bucket.
func (m *MetadataStore) DeleteAllObjectMetas(bucket string) error {
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
		return setObjectCount(tx, 0)
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
	return m.updateBucketInfo(bucket, func(info *BucketInfo) {
		info.Lifecycle = lc
	})
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
	return m.updateBucketInfo(bucket, func(info *BucketInfo) {
		info.Lifecycle = nil
	})
}

// PutBucketLogging sets logging configuration for a bucket.
func (m *MetadataStore) PutBucketLogging(bucket string, logging *BucketLoggingStatus) error {
	return m.updateBucketInfo(bucket, func(info *BucketInfo) {
		info.Logging = logging
	})
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
			if b.Get(objectMetaKey(info.Bucket, info.Key)) == nil {
				if err := adjustObjectCount(tx, 1); err != nil {
					return err
				}
			}
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
