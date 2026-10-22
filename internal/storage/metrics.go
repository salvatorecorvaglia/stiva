package storage

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MetricsTracker holds thread-safe global statistics about the server operations.
type MetricsTracker struct {
	latencyMu      sync.Mutex
	latencyCount   uint64
	latencySum     float64
	latencyBuckets [len(latencyBounds)]uint64

	RequestsTotal    uint64
	RequestErrors    uint64
	BytesUploaded    uint64
	BytesDownloaded  uint64
	ActiveMultiparts int64
	// WebhookDropped/SyncDropped count events that were never delivered:
	// discarded because their dispatcher's bounded queue was full, or
	// abandoned when shutdown hit its deadline. The only prior signal for
	// either was a log line, so an operator relying on /metrics had no way
	// to detect silent notification/replication loss.
	WebhookDropped uint64
	SyncDropped    uint64
	SyncFailures   uint64
	SyncPending    int64
	SyncLastScan   int64

	// Disk space caching
	lastDiskCheck time.Time
	cachedTotal   uint64
	cachedFree    uint64
	diskMu        sync.Mutex
}

// GlobalMetrics is the singleton instance of the metrics tracker.
var GlobalMetrics = &MetricsTracker{}

func (m *MetricsTracker) IncRequests() {
	atomic.AddUint64(&m.RequestsTotal, 1)
}

func (m *MetricsTracker) IncErrors() {
	atomic.AddUint64(&m.RequestErrors, 1)
}

// AddUploaded records n uploaded bytes. Negative values are ignored so that
// callers can pass io.Copy results directly without an unchecked conversion.
func (m *MetricsTracker) AddUploaded(n int64) {
	if n <= 0 {
		return
	}
	atomic.AddUint64(&m.BytesUploaded, uint64(n))
}

// AddDownloaded records n downloaded bytes. Negative values are ignored.
func (m *MetricsTracker) AddDownloaded(n int64) {
	if n <= 0 {
		return
	}
	atomic.AddUint64(&m.BytesDownloaded, uint64(n))
}

func (m *MetricsTracker) IncActiveMultiparts() {
	atomic.AddInt64(&m.ActiveMultiparts, 1)
}

func (m *MetricsTracker) DecActiveMultiparts() {
	atomic.AddInt64(&m.ActiveMultiparts, -1)
}

// SetActiveMultiparts sets the gauge outright, for seeding it from the uploads
// already in progress at startup.
func (m *MetricsTracker) SetActiveMultiparts(n int64) {
	atomic.StoreInt64(&m.ActiveMultiparts, n)
}

// IncWebhookDropped records a webhook event that will never be delivered:
// the dispatch queue was full, or shutdown abandoned it.
func (m *MetricsTracker) IncWebhookDropped() {
	atomic.AddUint64(&m.WebhookDropped, 1)
}

// IncSyncDropped records a replication task that will never be delivered:
// the dispatch queue was full, or shutdown abandoned it.
func (m *MetricsTracker) IncSyncDropped() {
	atomic.AddUint64(&m.SyncDropped, 1)
}

func (m *MetricsTracker) IncSyncFailure() { atomic.AddUint64(&m.SyncFailures, 1) }

// FormatPrometheus generates a plain-text Prometheus-compliant metrics string.
func (m *MetricsTracker) FormatPrometheus(dataDir string) string {
	m.diskMu.Lock()
	if time.Since(m.lastDiskCheck) > 30*time.Second {
		total, free, err := GetDiskSpace(dataDir)
		if err == nil {
			m.cachedTotal = total
			m.cachedFree = free
			m.lastDiskCheck = time.Now()
		}
	}
	diskTotalVal := m.cachedTotal
	diskFreeVal := m.cachedFree
	m.diskMu.Unlock()

	reqs := atomic.LoadUint64(&m.RequestsTotal)
	errs := atomic.LoadUint64(&m.RequestErrors)
	up := atomic.LoadUint64(&m.BytesUploaded)
	down := atomic.LoadUint64(&m.BytesDownloaded)
	activeMP := atomic.LoadInt64(&m.ActiveMultiparts)
	webhookDropped := atomic.LoadUint64(&m.WebhookDropped)
	syncDropped := atomic.LoadUint64(&m.SyncDropped)

	inodesTotal, inodesFree := getDiskInodes(dataDir)
	return m.formatLatency() + fmt.Sprintf(`# HELP stiva_disk_inodes_total Total inodes on the storage volume; zero if unsupported.
# TYPE stiva_disk_inodes_total gauge
stiva_disk_inodes_total %d
# HELP stiva_disk_inodes_free Free inodes on the storage volume; zero if unsupported.
# TYPE stiva_disk_inodes_free gauge
stiva_disk_inodes_free %d
# HELP stiva_sync_pending Number of objects awaiting replication at the start of the last scan.
# TYPE stiva_sync_pending gauge
stiva_sync_pending %d
# HELP stiva_sync_failures_total Failed durable replication attempts including SSE-C skips.
# TYPE stiva_sync_failures_total counter
stiva_sync_failures_total %d
# HELP stiva_sync_last_scan_timestamp_seconds Last completed durable replication scan.
# TYPE stiva_sync_last_scan_timestamp_seconds gauge
stiva_sync_last_scan_timestamp_seconds %d
# HELP stiva_requests_total Total number of S3 requests processed.
# TYPE stiva_requests_total counter
stiva_requests_total %d

# HELP stiva_request_errors_total Total number of S3 request errors.
# TYPE stiva_request_errors_total counter
stiva_request_errors_total %d

# HELP stiva_bytes_uploaded_total Total bytes uploaded to Stiva.
# TYPE stiva_bytes_uploaded_total counter
stiva_bytes_uploaded_total %d

# HELP stiva_bytes_downloaded_total Total bytes downloaded from Stiva.
# TYPE stiva_bytes_downloaded_total counter
stiva_bytes_downloaded_total %d

# HELP stiva_active_multipart_uploads Number of active multipart uploads.
# TYPE stiva_active_multipart_uploads gauge
stiva_active_multipart_uploads %d

# HELP stiva_disk_total_bytes Total disk space of the storage directory in bytes.
# TYPE stiva_disk_total_bytes gauge
stiva_disk_total_bytes %d

# HELP stiva_disk_free_bytes Free disk space of the storage directory in bytes.
# TYPE stiva_disk_free_bytes gauge
stiva_disk_free_bytes %d

# HELP stiva_webhook_dropped_total Total webhook events never delivered: discarded because the dispatch queue was full, or abandoned at shutdown.
# TYPE stiva_webhook_dropped_total counter
stiva_webhook_dropped_total %d

# HELP stiva_sync_dropped_total Total replication tasks never delivered: discarded because the dispatch queue was full, or abandoned at shutdown.
# TYPE stiva_sync_dropped_total counter
stiva_sync_dropped_total %d
`, inodesTotal, inodesFree, atomic.LoadInt64(&m.SyncPending), atomic.LoadUint64(&m.SyncFailures), atomic.LoadInt64(&m.SyncLastScan), reqs, errs, up, down, activeMP, diskTotalVal, diskFreeVal, webhookDropped, syncDropped)
}
