package storage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWebhookQueueFullIncrementsDroppedMetric guards against an observability
// gap: both dispatchers silently dropped tasks (aside from a log line) when
// their bounded queue was full, with no Prometheus counter an operator could
// alert on. This floods the queue past its capacity via a deliberately slow
// receiver and asserts the drop is now visible both as a counter and in the
// /metrics text output.
func TestWebhookQueueFullIncrementsDroppedMetric(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never respond until the test releases it
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tempDir := t.TempDir()
	engine, err := NewFilesystemEngine(tempDir, nil, server.URL)
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	// engine.Close() waits for in-flight webhook workers, which are stuck on
	// <-block until this releases them — must close(block) before Close(),
	// so register it as a defer *after* engine.Close() (LIFO: runs first).
	defer engine.Close()
	defer close(block)

	before := atomic.LoadUint64(&GlobalMetrics.WebhookDropped)

	info := &ObjectInfo{Bucket: "b", Key: "k"}
	// 10 workers pick up 10 events and block on the slow receiver; the next
	// 5000 fill the queue's buffer; anything beyond that must be dropped.
	const total = 5000 + 10 + 25
	for i := 0; i < total; i++ {
		engine.triggerWebhook("ObjectCreated:Put", info)
	}

	deadline := time.Now().Add(5 * time.Second)
	var dropped uint64
	for {
		dropped = atomic.LoadUint64(&GlobalMetrics.WebhookDropped) - before
		if dropped > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if dropped == 0 {
		t.Fatal("expected WebhookDropped to increment once the queue overflowed, got 0")
	}

	metricsText := GlobalMetrics.FormatPrometheus(tempDir)
	if !strings.Contains(metricsText, "stiva_webhook_dropped_total") {
		t.Error("expected stiva_webhook_dropped_total in /metrics output")
	}
}

// TestSyncQueueFullIncrementsDroppedMetric mirrors the webhook case for
// replication.
func TestSyncQueueFullIncrementsDroppedMetric(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &SyncConfig{
		Endpoint:  server.URL,
		Bucket:    "backup",
		AccessKey: "ak",
		SecretKey: "sk",
		Region:    "us-east-1",
	}

	tempDir := t.TempDir()
	engine, err := NewFilesystemEngine(tempDir, cfg, "")
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	// See TestWebhookQueueFullIncrementsDroppedMetric: close(block) must run
	// before engine.Close() waits on the stuck in-flight workers.
	defer engine.Close()
	defer close(block)

	before := atomic.LoadUint64(&GlobalMetrics.SyncDropped)

	const total = 5000 + 10 + 25
	for i := 0; i < total; i++ {
		// DELETE needs no local object to exist, unlike PUT.
		engine.MirrorSync("primary", "some-key", "DELETE")
	}

	deadline := time.Now().Add(5 * time.Second)
	var dropped uint64
	for {
		dropped = atomic.LoadUint64(&GlobalMetrics.SyncDropped) - before
		if dropped > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if dropped == 0 {
		t.Fatal("expected SyncDropped to increment once the queue overflowed, got 0")
	}

	metricsText := GlobalMetrics.FormatPrometheus(tempDir)
	if !strings.Contains(metricsText, "stiva_sync_dropped_total") {
		t.Error("expected stiva_sync_dropped_total in /metrics output")
	}
}
