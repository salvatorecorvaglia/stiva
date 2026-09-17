package storage

import "sync/atomic"

// Test-only accessors. These used to be exported methods on the production
// types, reachable only because the tests lived in a separate package; they
// belong here now that the tests are in-package.

func (m *MetadataStore) activeBucketsCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.activeBuckets)
}

func (m *MetadataStore) activeBucketRefCount(bucket string) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.activeBuckets[bucket]
	if !ok {
		return 0, false
	}
	return entry.refCount, true
}

func (m *MetadataStore) hasInitLock(bucket string) bool {
	m.initMu.Lock()
	defer m.initMu.Unlock()
	_, exists := m.initLocks[bucket]
	return exists
}

func (fs *FilesystemEngine) isSyncShuttingDownNow() bool {
	return atomic.LoadInt32(&fs.isSyncShuttingDown) == 1
}

func (fs *FilesystemEngine) isWebhookShuttingDownNow() bool {
	return atomic.LoadInt32(&fs.isWebhookShuttingDown) == 1
}

// newFilesystemEngineForTesting builds a FilesystemEngine without touching disk.
func newFilesystemEngineForTesting(dataDir string) *FilesystemEngine {
	return &FilesystemEngine{dataDir: dataDir}
}
