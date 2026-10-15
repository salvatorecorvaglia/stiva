package storage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestEngineShutdownRespectsDeadline is the regression test for shutdown
// draining the replication and webhook queues with no bound. Each queue holds
// up to 5,000 events and each delivery can wait on a 5-minute (replication)
// or 5-second (webhook) client timeout, so a slow or dead target held the
// process far past the 15s shutdown window. Past the deadline, in-flight
// deliveries are cancelled and whatever is left is abandoned and counted in
// the dropped-event metrics.
func TestEngineShutdownRespectsDeadline(t *testing.T) {
	// A target that never answers: it holds each request until the client
	// gives up on it.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	engine, err := NewFilesystemEngine(t.TempDir(), &SyncConfig{
		Endpoint: server.URL, Bucket: "backup", AccessKey: "ak", SecretKey: "sk", Region: "us-east-1",
	}, server.URL)
	if err != nil {
		t.Fatal(err)
	}

	syncBefore := atomic.LoadUint64(&GlobalMetrics.SyncDropped)
	webhookBefore := atomic.LoadUint64(&GlobalMetrics.WebhookDropped)

	// 10 workers each take one event and hang; the rest wait in the queue.
	const events = 50
	for i := 0; i < events; i++ {
		engine.MirrorSync("primary", "k", "DELETE")
		engine.triggerWebhook("ObjectCreated:Put", &ObjectInfo{Bucket: "primary", Key: "k"})
	}
	time.Sleep(100 * time.Millisecond) // let the workers pick up their first events

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = engine.Shutdown(ctx)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v with a 300ms deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want it to report the missed deadline", err)
	}

	// Every event that never reached its target has to show up in /metrics.
	if got := atomic.LoadUint64(&GlobalMetrics.SyncDropped) - syncBefore; got != events {
		t.Errorf("SyncDropped rose by %d, want %d (every undelivered replication event)", got, events)
	}
	if got := atomic.LoadUint64(&GlobalMetrics.WebhookDropped) - webhookBefore; got != events {
		t.Errorf("WebhookDropped rose by %d, want %d (every undelivered webhook event)", got, events)
	}
}

// TestEngineShutdownWithoutDeadlineStillDrains: a shutdown with time to spare
// delivers everything queued, as before; only a missed deadline abandons work.
func TestEngineShutdownWithoutDeadlineStillDrains(t *testing.T) {
	var delivered atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	engine, err := NewFilesystemEngine(t.TempDir(), nil, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	const events = 100
	for i := 0; i < events; i++ {
		engine.triggerWebhook("ObjectCreated:Put", &ObjectInfo{Bucket: "b", Key: "k"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := delivered.Load(); got != events {
		t.Errorf("delivered %d of %d queued webhook events before shutdown returned", got, events)
	}
}
