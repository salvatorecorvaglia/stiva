package storage

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

// TestActiveMultipartGaugeSeededAtStartup is the regression test for
// stiva_active_multipart_uploads going negative. The gauge started at zero on
// every boot, so aborting or completing uploads created before a restart
// drove it below zero.
func TestActiveMultipartGaugeSeededAtStartup(t *testing.T) {
	dir := t.TempDir()
	engine, err := NewFilesystemEngine(dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []string{"gauge-a", "gauge-b"} {
		if err := engine.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.CreateMultipartUpload(context.Background(), bucket, "big.bin", "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	atomic.StoreInt64(&GlobalMetrics.ActiveMultiparts, 0)
	reopened, err := NewFilesystemEngine(dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	if got := atomic.LoadInt64(&GlobalMetrics.ActiveMultiparts); got != 2 {
		t.Fatalf("active multipart gauge after restart = %d, want 2", got)
	}
}

// TestEngineRecordsIntoInjectedMetrics: every counter used to go to the
// package-level GlobalMetrics, so two engines in one process (or tests run
// side by side) mixed their numbers. An engine given its own tracker records
// only there.
func TestEngineRecordsIntoInjectedMetrics(t *testing.T) {
	engine := setupTestEngine(t)
	own := &MetricsTracker{}
	engine.SetMetrics(own)
	if err := engine.CreateBucket("own-metrics"); err != nil {
		t.Fatal(err)
	}

	globalBefore := atomic.LoadUint64(&GlobalMetrics.BytesUploaded)
	if _, err := engine.PutObject(context.Background(), "own-metrics", "k", strings.NewReader("12345"), 5, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	r, _, err := engine.GetObject(context.Background(), "own-metrics", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(r)
	r.Close()
	if _, err := engine.CreateMultipartUpload(context.Background(), "own-metrics", "mp", "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadUint64(&own.BytesUploaded); got != 5 {
		t.Errorf("injected tracker BytesUploaded = %d, want 5", got)
	}
	if got := atomic.LoadUint64(&own.BytesDownloaded); got != 5 {
		t.Errorf("injected tracker BytesDownloaded = %d, want 5", got)
	}
	if got := atomic.LoadInt64(&own.ActiveMultiparts); got != 1 {
		t.Errorf("injected tracker ActiveMultiparts = %d, want 1", got)
	}
	if atomic.LoadUint64(&GlobalMetrics.BytesUploaded) != globalBefore {
		t.Error("the engine still recorded into GlobalMetrics")
	}
}
