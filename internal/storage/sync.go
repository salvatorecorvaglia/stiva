package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
)

// SyncConfig holds S3 replication target credentials and endpoint.
type SyncConfig struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
}

type syncTask struct {
	fs     *FilesystemEngine
	cfg    *SyncConfig
	bucket string
	key    string
	op     string
}

// errSkipSSEC marks an object that is deliberately not replicated because it
// is encrypted with a customer-provided key Stiva does not retain. It is not a
// transient failure: without the key the object cannot be read here at all, so
// every retry would fail identically.
var errSkipSSEC = errors.New("object is SSE-C encrypted; the customer key is not retained, so it cannot be replicated")

const syncQueueSize = 5000
const syncWorkerCount = 10
const (
	syncMaxRetries  = 3
	syncBaseBackoff = time.Second
)

func (fs *FilesystemEngine) initSyncDispatcher() {
	fs.syncQueue = make(chan syncTask, syncQueueSize)
	for i := 0; i < syncWorkerCount; i++ {
		fs.syncWG.Add(1)
		go func() {
			defer fs.syncWG.Done()
			for task := range fs.syncQueue {
				fs.attemptSync(task, 0)
			}
		}()
	}
}

// attemptSync performs one replication attempt for task and, on failure,
// schedules retries with exponential backoff (matching the webhook
// dispatcher). Unlike a single unconditional attempt, this means a transient
// network blip or remote 5xx no longer permanently drops the replication
// event. Each scheduled retry is tracked by fs.syncWG so StopSyncDispatcher's
// Wait() blocks until the retry chain finishes rather than returning while a
// retry is still pending.
func (fs *FilesystemEngine) attemptSync(task syncTask, n int) {
	err := performSync(context.Background(), task.fs, task.fs.syncClient, task.cfg, task.bucket, task.key, task.op)
	if err == nil {
		slog.Info("[Sync] Mirroring succeeded", "op", task.op, "bucket", task.bucket, "key", task.key)
		return
	}

	// Retrying cannot help, and reporting it as a failure three times buried
	// the real message: this object will be missing from the mirror.
	if errors.Is(err, errSkipSSEC) {
		slog.Warn("[Sync] Skipping SSE-C encrypted object; the mirror will not contain it",
			"op", task.op, "bucket", task.bucket, "key", task.key)
		return
	}

	if atomic.LoadInt32(&fs.isSyncShuttingDown) == 1 {
		slog.Warn("[Sync] Dispatcher shutting down, abandoning retry", "op", task.op, "bucket", task.bucket, "key", task.key)
		return
	}
	if n >= syncMaxRetries {
		slog.Error("[Sync] Mirroring failed after retries", "op", task.op, "bucket", task.bucket, "key", task.key, "retries", syncMaxRetries, "error", err)
		return
	}

	slog.Warn("[Sync] Mirroring attempt failed, will retry", "op", task.op, "bucket", task.bucket, "key", task.key, "attempt", n, "error", err)
	backoff := syncBaseBackoff << n
	fs.syncWG.Add(1)
	time.AfterFunc(backoff, func() {
		defer fs.syncWG.Done()
		fs.attemptSync(task, n+1)
	})
}

// StopSyncDispatcher halts replication queue processing, waits for pending transfers, and shuts down workers.
func (fs *FilesystemEngine) StopSyncDispatcher() {
	fs.syncMu.Lock()
	if atomic.SwapInt32(&fs.isSyncShuttingDown, 1) == 1 {
		fs.syncMu.Unlock()
		return
	}

	q := fs.syncQueue
	fs.syncMu.Unlock()

	if q != nil {
		close(q)
		fs.syncWG.Wait()
	}
}

// MirrorSync schedules an async mirroring operation to the backup S3 bucket.
func (fs *FilesystemEngine) MirrorSync(bucket, key, op string) {
	if fs.syncConfig == nil {
		return
	}

	fs.syncMu.Lock()
	defer fs.syncMu.Unlock()

	if atomic.LoadInt32(&fs.isSyncShuttingDown) == 1 {
		slog.Warn("[Sync] Sync dispatcher shutting down, ignoring replication request", "op", op, "bucket", bucket, "key", key)
		return
	}

	fs.syncOnce.Do(func() {
		fs.initSyncDispatcher()
	})

	task := syncTask{
		fs:     fs,
		cfg:    fs.syncConfig,
		bucket: bucket,
		key:    key,
		op:     op,
	}

	select {
	case fs.syncQueue <- task:
	default:
		GlobalMetrics.IncSyncDropped()
		slog.Warn("[Sync] Sync queue full, dropping sync task", "op", op, "bucket", bucket, "key", key)
	}
}

func performSync(ctx context.Context, fs *FilesystemEngine, client *http.Client, cfg *SyncConfig, bucket, key, op string) error {
	// Construct the destination URL.
	//
	// The source bucket is part of the destination key. Every source bucket
	// used to be flattened into cfg.Bucket under the bare object key, so two
	// buckets holding the same key silently overwrote each other on the
	// mirror — the replica lost data with no error anywhere.
	//
	// Each path segment is escaped individually: interpolating the raw key meant
	// that any key containing '?', '#' or a space produced a malformed request
	// whose path no longer matched the one being signed.
	destURL := fmt.Sprintf("%s/%s/%s", cfg.Endpoint, url.PathEscape(cfg.Bucket), escapeObjectKey(bucket+"/"+key))

	var req *http.Request
	var err error

	switch op {
	case "DELETE":
		req, err = http.NewRequestWithContext(ctx, http.MethodDelete, destURL, nil)
		if err != nil {
			return err
		}
	case "PUT":
		info, metaErr := fs.metadata.GetObjectMeta(bucket, key, "")
		if metaErr != nil {
			return fmt.Errorf("failed to fetch object metadata: %w", metaErr)
		}

		// GetObject below is called without SSE-C parameters, because the
		// customer key is deliberately never persisted. For an encrypted
		// object it therefore always fails; detect that here so it is
		// reported once, as a skip, rather than three times as a failure.
		if info.SSECustomerAlgorithm != "" {
			return errSkipSSEC
		}

		reader, _, readErr := fs.GetObject(ctx, bucket, key, "")
		if readErr != nil {
			return fmt.Errorf("failed to get object reader: %w", readErr)
		}
		defer reader.Close()

		bufferedReader := &bufferedReadCloser{
			Reader: bufio.NewReaderSize(reader, 64*1024),
			Closer: reader,
		}

		req, err = http.NewRequestWithContext(ctx, http.MethodPut, destURL, bufferedReader)
		if err != nil {
			return err
		}
		req.ContentLength = info.Size
		req.Header.Set("Content-Length", strconv.FormatInt(info.Size, 10))
		if info.ContentType != "" {
			req.Header.Set("Content-Type", info.ContentType)
		}
	default:
		return fmt.Errorf("unsupported sync operation: %s", op)
	}

	// Sign request
	auth.SignRequest(req, cfg.AccessKey, cfg.SecretKey, cfg.Region, "s3")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

type bufferedReadCloser struct {
	*bufio.Reader
	io.Closer
}

// escapeObjectKey percent-encodes an object key for use in a URL path, escaping
// each '/'-separated segment while preserving the separators themselves.
func escapeObjectKey(key string) string {
	segments := strings.Split(strings.TrimPrefix(key, "/"), "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}
