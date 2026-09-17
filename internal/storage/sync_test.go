package storage

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncConfigFromEnv builds a replication config from the STIVA_SYNC_* variables.
func syncConfigFromEnv() *SyncConfig {
	endpoint := os.Getenv("STIVA_SYNC_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	region := os.Getenv("STIVA_SYNC_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return &SyncConfig{
		Endpoint:  strings.TrimSuffix(endpoint, "/"),
		Bucket:    os.Getenv("STIVA_SYNC_BUCKET"),
		AccessKey: os.Getenv("STIVA_SYNC_ACCESS_KEY"),
		SecretKey: os.Getenv("STIVA_SYNC_SECRET_KEY"),
		Region:    region,
	}
}

func TestMirrorSync_Integration(t *testing.T) {
	var mu sync.Mutex
	type reqRecord struct {
		Method string
		URI    string
		Auth   string
		Body   string
	}
	var receivedReqs []reqRecord

	// Mock backup target S3 service
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedReqs = append(receivedReqs, reqRecord{
			Method: r.Method,
			URI:    r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Body:   string(bodyBytes),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Set env config for sync replication
	t.Setenv("STIVA_SYNC_ENDPOINT", server.URL)
	t.Setenv("STIVA_SYNC_BUCKET", "backup-bucket")
	t.Setenv("STIVA_SYNC_ACCESS_KEY", "backupaccess")
	t.Setenv("STIVA_SYNC_SECRET_KEY", "backupsecret")
	t.Setenv("STIVA_SYNC_REGION", "us-west-2")

	tempDir := t.TempDir()
	engine, err := NewFilesystemEngine(tempDir, syncConfigFromEnv(), "")
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	defer engine.Close()

	// Create bucket
	err = engine.CreateBucket("primary-bucket")
	if err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// 1. Trigger mirroring via PutObject
	content := "mirror-replication-payload"
	_, err = engine.PutObject(context.Background(), "primary-bucket", "docs/report.txt", strings.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Wait for the PUT request to be received before deleting, to ensure the async worker
	// has read the file for mirroring before it gets deleted from primary
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		count := len(receivedReqs)
		mu.Unlock()
		if count >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for PUT mirroring request")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 2. Trigger mirroring via DeleteObject
	_, _, err = engine.DeleteObject("primary-bucket", "docs/report.txt", "")
	if err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Wait for async mirroring worker to complete the DELETE request
	deadline = time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		count := len(receivedReqs)
		mu.Unlock()
		if count >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for DELETE mirroring request")
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(receivedReqs) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(receivedReqs))
	}

	putReq := receivedReqs[0]
	if putReq.Method != "PUT" {
		t.Errorf("expected PUT method, got %q", putReq.Method)
	}
	if putReq.URI != "/backup-bucket/primary-bucket/docs/report.txt" {
		t.Errorf("expected destination URI '/backup-bucket/primary-bucket/docs/report.txt', got %q", putReq.URI)
	}
	if putReq.Body != content {
		t.Errorf("expected body %q, got %q", content, putReq.Body)
	}
	if !strings.HasPrefix(putReq.Auth, "AWS4-HMAC-SHA256") {
		t.Errorf("expected AWS SigV4 authorization header, got %q", putReq.Auth)
	}

	deleteReq := receivedReqs[1]
	if deleteReq.Method != "DELETE" {
		t.Errorf("expected DELETE method, got %q", deleteReq.Method)
	}
	if deleteReq.URI != "/backup-bucket/primary-bucket/docs/report.txt" {
		t.Errorf("expected destination URI '/backup-bucket/primary-bucket/docs/report.txt', got %q", deleteReq.URI)
	}
}

// TestMirrorSyncRetriesOnTransientFailure guards against a gap where
// MirrorSync made exactly one delivery attempt: any transient failure (a
// remote 5xx, a dropped connection) permanently dropped that replication
// event with only a log line, silently diverging the backup bucket from the
// primary. The backup endpoint here fails the first attempt and succeeds on
// the retry.
func TestMirrorSyncRetriesOnTransientFailure(t *testing.T) {
	var mu sync.Mutex
	var attempts int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("STIVA_SYNC_ENDPOINT", server.URL)
	t.Setenv("STIVA_SYNC_BUCKET", "backup-bucket")
	t.Setenv("STIVA_SYNC_ACCESS_KEY", "backupaccess")
	t.Setenv("STIVA_SYNC_SECRET_KEY", "backupsecret")
	t.Setenv("STIVA_SYNC_REGION", "us-west-2")

	tempDir := t.TempDir()
	engine, err := NewFilesystemEngine(tempDir, syncConfigFromEnv(), "")
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	defer engine.Close()

	if err := engine.CreateBucket("primary-bucket"); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	content := "retry-me"
	_, err = engine.PutObject(context.Background(), "primary-bucket", "retry.txt", strings.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for retried mirroring attempt (saw %d attempt(s))", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSyncDispatcherShutdownWaitsForPendingRetry guards against a gap where a
// scheduled retry timer was not tracked by the dispatcher's WaitGroup:
// StopSyncDispatcher (and therefore Server.Shutdown) could report a clean
// shutdown while a retry — and its outbound HTTP call — was still pending.
// The backup endpoint here always fails, so a retry is always scheduled;
// engine.Close() must not return before that scheduled attempt has run.
func TestSyncDispatcherShutdownWaitsForPendingRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	t.Setenv("STIVA_SYNC_ENDPOINT", server.URL)
	t.Setenv("STIVA_SYNC_BUCKET", "backup-bucket")
	t.Setenv("STIVA_SYNC_ACCESS_KEY", "backupaccess")
	t.Setenv("STIVA_SYNC_SECRET_KEY", "backupsecret")
	t.Setenv("STIVA_SYNC_REGION", "us-west-2")

	tempDir := t.TempDir()
	engine, err := NewFilesystemEngine(tempDir, syncConfigFromEnv(), "")
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}

	if err := engine.CreateBucket("primary-bucket"); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	content := "shutdown-me"
	_, err = engine.PutObject(context.Background(), "primary-bucket", "shutdown.txt", strings.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Give the first (always-failing) attempt time to run and schedule its
	// retry timer before we shut down.
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	if err := engine.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	elapsed := time.Since(start)

	// The retry backoff for the first retry is ~1s; if Close() returned
	// almost instantly, the pending timer was not tracked and this shutdown
	// did not actually wait for it.
	if elapsed < 700*time.Millisecond {
		t.Errorf("Close() returned after %v, expected it to block for roughly the pending retry's backoff (~1s) — a scheduled sync retry is escaping shutdown tracking", elapsed)
	}
}

// TestMirrorSyncKeepsSourceBucketsApart covers a replica data-loss bug: every
// source bucket was flattened into the single destination bucket under the
// bare object key, so two buckets holding the same key overwrote each other on
// the mirror with no error reported anywhere.
func TestMirrorSyncKeepsSourceBucketsApart(t *testing.T) {
	var mu sync.Mutex
	var paths []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("STIVA_SYNC_ENDPOINT", server.URL)
	t.Setenv("STIVA_SYNC_BUCKET", "backup-bucket")
	t.Setenv("STIVA_SYNC_ACCESS_KEY", "ak")
	t.Setenv("STIVA_SYNC_SECRET_KEY", "sk")

	engine, err := NewFilesystemEngine(t.TempDir(), syncConfigFromEnv(), "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer engine.Close()

	// The same key in two different source buckets.
	for _, b := range []string{"alpha", "beta"} {
		if err := engine.CreateBucket(b); err != nil {
			t.Fatalf("create bucket %s: %v", b, err)
		}
		body := b + "-content"
		if _, err := engine.PutObject(context.Background(), b, "shared/logo.png",
			strings.NewReader(body), int64(len(body)), "image/png"); err != nil {
			t.Fatalf("put into %s: %v", b, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(paths)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for both mirroring requests, got %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		seen[p] = true
	}
	for _, want := range []string{
		"/backup-bucket/alpha/shared/logo.png",
		"/backup-bucket/beta/shared/logo.png",
	} {
		if !seen[want] {
			t.Errorf("missing mirrored path %q (got %v)", want, paths)
		}
	}
	if len(seen) != 2 {
		t.Errorf("source buckets collided on the mirror: %v", paths)
	}
}

// TestMirrorSyncSkipsSSECObjects covers a silent replication gap. performSync
// reads the source with no SSE-C parameters, because the customer key is never
// persisted, so an encrypted object could never be read and every attempt
// failed identically. The old code treated that as a transient failure and
// scheduled the full retry chain (1s, 2s, 4s backoff), which logged three
// misleading errors and — because each pending retry is tracked by syncWG —
// stalled engine shutdown behind timers that could never succeed.
func TestMirrorSyncSkipsSSECObjects(t *testing.T) {
	var mu sync.Mutex
	var requests int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("STIVA_SYNC_ENDPOINT", server.URL)
	t.Setenv("STIVA_SYNC_BUCKET", "backup-bucket")
	t.Setenv("STIVA_SYNC_ACCESS_KEY", "ak")
	t.Setenv("STIVA_SYNC_SECRET_KEY", "sk")

	engine, err := NewFilesystemEngine(t.TempDir(), syncConfigFromEnv(), "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	if err := engine.CreateBucket("enc"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	sum := md5.Sum(key)
	ctx := context.WithValue(context.Background(), SSECContextKey, &SSECParams{
		Algorithm: "AES256",
		Key:       key,
		KeyMD5:    base64.StdEncoding.EncodeToString(sum[:]),
	})

	body := "secret-payload"
	if _, err := engine.PutObject(ctx, "enc", "secret.bin",
		strings.NewReader(body), int64(len(body)), "application/octet-stream"); err != nil {
		t.Fatalf("put encrypted object: %v", err)
	}

	// Let the dispatcher pick the task up and decide what to do with it.
	time.Sleep(300 * time.Millisecond)

	// With the skip in place nothing is pending, so shutdown is immediate.
	// Without it, a retry is already armed behind a 1s backoff and Close()
	// has to wait for that timer to fire before syncWG drains.
	start := time.Now()
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("shutdown blocked for %v waiting on retries for an object that can never be replicated", elapsed)
	}

	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Errorf("SSE-C object should not be sent to the mirror, got %d request(s)", requests)
	}
}
