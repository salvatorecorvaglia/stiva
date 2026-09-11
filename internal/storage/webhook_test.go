package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWebhook_Integration(t *testing.T) {
	var mu sync.Mutex
	var receivedPayloads []WebhookPayload

	// Create mock webhook receiver
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload WebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		receivedPayloads = append(receivedPayloads, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Set env
	t.Setenv("STIVA_WEBHOOK_URL", server.URL)

	tempDir := t.TempDir()
	engine, err := NewFilesystemEngine(tempDir, nil, server.URL)
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	defer engine.Close()

	// Create bucket
	err = engine.CreateBucket("webhooks")
	if err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	// Put object to trigger event
	content := "webhook-test-payload"
	_, err = engine.PutObject(context.Background(), "webhooks", "test.txt", strings.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Wait for the PUT webhook event to be delivered
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		count := len(receivedPayloads)
		mu.Unlock()
		if count >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for PUT webhook payload")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Delete object to trigger delete event
	_, _, err = engine.DeleteObject("webhooks", "test.txt", "")
	if err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Wait for the DELETE webhook event to be delivered
	deadline = time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		count := len(receivedPayloads)
		mu.Unlock()
		if count >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for DELETE webhook payload")
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(receivedPayloads) < 2 {
		t.Fatalf("expected at least 2 webhook payloads, got %d", len(receivedPayloads))
	}

	putEvent := receivedPayloads[0]
	if putEvent.EventName != "s3:ObjectCreated:Put" {
		t.Errorf("expected event s3:ObjectCreated:Put, got %q", putEvent.EventName)
	}
	if putEvent.Bucket != "webhooks" || putEvent.Key != "test.txt" {
		t.Errorf("unexpected event source info: %+v", putEvent)
	}

	deleteEvent := receivedPayloads[1]
	if deleteEvent.EventName != "s3:ObjectRemoved:Delete" {
		t.Errorf("expected event s3:ObjectRemoved:Delete, got %q", deleteEvent.EventName)
	}
}

// TestWebhookDispatcherShutdownWaitsForPendingRetry guards against a gap
// where a scheduled webhook retry timer (time.AfterFunc) was not tracked by
// the dispatcher's WaitGroup: StopWebhookDispatcher (and therefore
// Server.Shutdown) could report a clean shutdown while a retry — and its
// outbound HTTP call — was still pending, up to several seconds after the
// process claimed to have stopped. The receiver here always fails, so a
// retry is always scheduled; engine.Close() must not return before that
// scheduled attempt has run.
func TestWebhookDispatcherShutdownWaitsForPendingRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	t.Setenv("STIVA_WEBHOOK_URL", server.URL)

	tempDir := t.TempDir()
	engine, err := NewFilesystemEngine(tempDir, nil, server.URL)
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}

	if err := engine.CreateBucket("webhooks"); err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	content := "shutdown-me"
	_, err = engine.PutObject(context.Background(), "webhooks", "shutdown.txt", strings.NewReader(content), int64(len(content)), "text/plain")
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
		t.Errorf("Close() returned after %v, expected it to block for roughly the pending retry's backoff (~1s) — a scheduled webhook retry is escaping shutdown tracking", elapsed)
	}
}
