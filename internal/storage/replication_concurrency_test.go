package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPendingDeliveryDoesNotBlockOverwriteOrAcknowledgeNewRevision(t *testing.T) {
	entered := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(unblock) })
	var mu sync.Mutex
	var contents []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		contents = append(contents, string(body))
		first := len(contents) == 1
		mu.Unlock()
		if first {
			close(entered)
			<-unblock
		}
	}))
	defer target.Close()
	engine, err := NewFilesystemEngine(t.TempDir(), &SyncConfig{Endpoint: target.URL, Bucket: "mirror", Region: "us-east-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	defer once.Do(func() { close(unblock) })
	engine.durableReplication = true
	if err := engine.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PutObject(context.Background(), "source", "key", strings.NewReader("old"), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
	reconciled := make(chan error, 1)
	go func() { reconciled <- engine.reconcilePending(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery did not start")
	}
	updated := make(chan error, 1)
	go func() {
		_, err := engine.PutObject(context.Background(), "source", "key", strings.NewReader("new"), 3, "text/plain")
		updated <- err
	}()
	select {
	case err := <-updated:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		once.Do(func() { close(unblock) })
		t.Fatal("remote response blocked local overwrite")
	}
	once.Do(func() { close(unblock) })
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	if n := pendingCount(t, engine, "source"); n != 1 {
		t.Fatalf("new revision was acknowledged: pending=%d", n)
	}
	if err := engine.reconcilePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(contents) != 2 || contents[0] != "old" || contents[1] != "new" {
		t.Fatalf("deliveries=%v", contents)
	}
}

func TestPendingDeliveryConcurrencyIsBounded(t *testing.T) {
	entered := make(chan struct{}, 10)
	unblock := make(chan struct{})
	var once sync.Once
	var active, maximum atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-unblock
	}))
	defer target.Close()
	defer once.Do(func() { close(unblock) })
	engine, err := NewFilesystemEngine(t.TempDir(), &SyncConfig{Endpoint: target.URL, Bucket: "mirror", Region: "us-east-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	defer once.Do(func() { close(unblock) })
	engine.durableReplication = true
	if err := engine.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if _, err := engine.PutObject(context.Background(), "source", key, strings.NewReader("data"), 4, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- engine.reconcilePending(context.Background()) }()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			once.Do(func() { close(unblock) })
			t.Fatal("independent keys did not progress concurrently")
		}
	}
	once.Do(func() { close(unblock) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := maximum.Load(); n != 4 {
		t.Fatalf("maximum concurrent requests=%d, want 4", n)
	}
	if n := pendingCount(t, engine, "source"); n != 0 {
		t.Fatalf("pending=%d", n)
	}
}

// The same durable reconciler runs against a target with 40 ms of response
// latency, using either one or four remote connections. This isolates the
// benefit of overlapping independent keys from local publication throughput.
func BenchmarkReplicationSlowTarget(b *testing.B) {
	for _, connections := range []int{1, 4} {
		b.Run(fmt.Sprintf("connections-%d", connections), func(b *testing.B) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				time.Sleep(40 * time.Millisecond)
			}))
			b.Cleanup(target.Close)
			engine, err := NewFilesystemEngine(b.TempDir(), &SyncConfig{Endpoint: target.URL, Bucket: "mirror", Region: "us-east-1"}, "")
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { engine.Close() })
			engine.durableReplication = true
			engine.SetMetrics(&MetricsTracker{})
			engine.syncClient.Transport.(*http.Transport).MaxConnsPerHost = connections
			if err := engine.CreateBucket("source"); err != nil {
				b.Fatal(err)
			}
			for _, key := range []string{"a", "b", "c", "d"} {
				if _, err := engine.PutObject(context.Background(), "source", key, strings.NewReader("payload"), 7, "text/plain"); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := engine.seedReplication(); err != nil {
					b.Fatal(err)
				}
				if err := engine.reconcilePending(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
