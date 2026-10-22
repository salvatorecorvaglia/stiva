package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func pendingCount(t *testing.T, fs *FilesystemEngine, bucket string) int {
	t.Helper()
	db, release, err := fs.metadata.acquireBucketDB(bucket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	n := 0
	if err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(replicationBucket); b != nil {
			n = b.Stats().KeyN
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDurableReplicationSurvivesRestartAndRemoteFailure(t *testing.T) {
	var mu sync.Mutex
	fail := true
	remote := map[string]string{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(503)
			return
		}
		switch r.Method {
		case "PUT":
			body, _ := io.ReadAll(r.Body)
			remote[r.URL.Path] = string(body)
		case "DELETE":
			delete(remote, r.URL.Path)
		}
	}))
	defer target.Close()
	cfg := &SyncConfig{Endpoint: target.URL, Bucket: "mirror", AccessKey: "access", SecretKey: "secret", Region: "us-east-1"}
	data := t.TempDir()
	fs, err := NewFilesystemEngine(data, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	// Select durable dispatch without starting its timer, so crash recovery can
	// be exercised deterministically using the same persisted metadata.
	fs.durableReplication = true
	if err := fs.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.PutObject(context.Background(), "source", "key", strings.NewReader("first"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if n := pendingCount(t, fs, "source"); n != 1 {
		t.Fatalf("pending=%d", n)
	}
	if err := fs.reconcilePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pendingCount(t, fs, "source"); n != 1 {
		t.Fatal("remote failure cleared pending record")
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	fs, err = NewFilesystemEngine(data, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	fs.durableReplication = true
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := fs.reconcilePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pendingCount(t, fs, "source"); n != 0 {
		t.Fatalf("pending after recovery=%d", n)
	}
	mu.Lock()
	got := remote["/mirror/source/key"]
	mu.Unlock()
	if got != "first" {
		t.Fatalf("replica=%q", got)
	}
	if _, _, err := fs.DeleteObject("source", "key", ""); err != nil {
		t.Fatal(err)
	}
	if err := fs.DeleteBucket("source"); err == nil {
		t.Fatal("deleted bucket with pending tombstones")
	}
	if err := fs.reconcilePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	_, exists := remote["/mirror/source/key"]
	mu.Unlock()
	if exists {
		t.Fatal("remote deletion lost")
	}
	if err := fs.DeleteBucket("source"); err != nil {
		t.Fatal(err)
	}
}

func TestDurableReplicationStartsAndStops(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	fs, err := NewFilesystemEngine(t.TempDir(), &SyncConfig{Endpoint: target.URL, Bucket: "mirror", Region: "us-east-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	fs.StartDurableReplication()
	fs.StartDurableReplication()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fs.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReadinessDetectsUnavailableStorage(t *testing.T) {
	fs, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CheckReady(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	fs.readyAt = time.Time{}
	if err := fs.CheckReady(); err == nil {
		t.Fatal("closed metadata reported ready")
	}
}

func TestDurableCommitRejectsMissingDirectory(t *testing.T) {
	fs, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if err := fs.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(fs.dataDir + "/blobs"); err != nil {
		t.Fatal(err)
	}
	// Simulate failure at the rename/directory-sync boundary. No metadata may
	// become visible after the data path fails.
	err = fs.commitObject("missing", "0123456789abcdef0123456789abcdef", fs.dataDir+"/blobs/missing", &ObjectInfo{Bucket: "source", Key: "key"})
	if err == nil {
		t.Fatal("failed commit succeeded")
	}
	if _, err := fs.metadata.GetObjectMeta("source", "key", ""); err == nil {
		t.Fatal("failed commit published metadata")
	}
}

func TestPartReplacementAndLegacyRecovery(t *testing.T) {
	data := t.TempDir()
	fs, err := NewFilesystemEngine(data, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	upload, err := fs.CreateMultipartUpload(context.Background(), "source", "object", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	first, err := fs.UploadPart(context.Background(), "source", "object", upload.UploadID, 1, strings.NewReader("first"), 5)
	if err != nil {
		t.Fatal(err)
	}
	partDir, err := fs.multipartDir("source", "object", upload.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, err := partDataPath(partDir, *first)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the first part to the historical numbered layout, then reopen.
	legacy := *first
	legacy.BlobID = ""
	legacyPath, err := partDataPath(partDir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(firstPath, legacyPath); err != nil {
		t.Fatal(err)
	}
	meta, err := fs.metadata.GetMultipartMeta("source", "object", upload.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	meta.Parts = []PartInfo{legacy}
	if err := fs.metadata.PutMultipartMeta(meta); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	fs, err = NewFilesystemEngine(data, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	second, err := fs.UploadPart(context.Background(), "source", "object", upload.UploadID, 1, strings.NewReader("replacement"), 11)
	if err != nil {
		t.Fatal(err)
	}
	if second.BlobID == "" || second.BlobID == first.BlobID {
		t.Fatal("part replacement did not get immutable storage")
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("replaced part retained: %v", err)
	}
	if _, err := fs.CompleteMultipartUpload(context.Background(), "source", "object", upload.UploadID, []CompletePart{{PartNumber: 1, ETag: second.ETag}}); err != nil {
		t.Fatal(err)
	}
	r, _, err := fs.GetObject(context.Background(), "source", "object", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("completed object=%q, %v", got, err)
	}
}

func TestDurableWorkerBackfillsExistingObjects(t *testing.T) {
	data := t.TempDir()
	fs, err := NewFilesystemEngine(data, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.PutObject(context.Background(), "source", "existing", strings.NewReader("backfill"), 8, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 10)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivered <- string(body)
		w.WriteHeader(200)
	}))
	defer target.Close()
	fs, err = NewFilesystemEngine(data, &SyncConfig{Endpoint: target.URL, Bucket: "mirror", Region: "us-east-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	fs.SetMetrics(&MetricsTracker{})
	fs.StartDurableReplication()
	select {
	case got := <-delivered:
		if got != "backfill" {
			t.Fatalf("backfill=%q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("existing object was not backfilled")
	}
	deadline := time.Now().Add(3 * time.Second)
	for pendingCount(t, fs, "source") != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := pendingCount(t, fs, "source"); n != 0 {
		t.Fatalf("delivered entry remained pending: %d", n)
	}
	text := fs.Metrics().FormatPrometheus(data)
	if !strings.Contains(text, "stiva_sync_last_scan_timestamp_seconds") {
		t.Fatal("replication metrics missing")
	}
}

func TestReplicationAcknowledgementDoesNotClearNewerRevision(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	fs, err := NewFilesystemEngine(t.TempDir(), &SyncConfig{Endpoint: target.URL, Bucket: "mirror", Region: "us-east-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	fs.durableReplication = true
	if err := fs.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	put := func(value string) {
		t.Helper()
		if _, err := fs.PutObject(context.Background(), "source", "key", strings.NewReader(value), int64(len(value)), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	put("before")
	db, release, err := fs.metadata.acquireBucketDB("source")
	if err != nil {
		t.Fatal(err)
	}
	var old pendingReplica
	if err := db.View(func(tx *bolt.Tx) error {
		old = pendingReplica{key: "key", revision: append([]byte(nil), tx.Bucket(replicationBucket).Get([]byte("key"))...)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	release()
	put("after")
	if err := fs.deliverPending(context.Background(), "source", old); err != nil {
		t.Fatal(err)
	}
	if n := pendingCount(t, fs, "source"); n != 1 {
		t.Fatal("old delivery cleared a newer revision")
	}
	if err := fs.reconcilePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pendingCount(t, fs, "source"); n != 0 {
		t.Fatal("latest revision not acknowledged")
	}
}

func TestLatencyHistogram(t *testing.T) {
	m := &MetricsTracker{}
	m.ObserveLatency(10 * time.Millisecond)
	m.ObserveLatency(2 * time.Second)
	text := m.FormatPrometheus(t.TempDir())
	for _, want := range []string{`stiva_request_duration_seconds_bucket{le="0.01"} 1`, `stiva_request_duration_seconds_bucket{le="+Inf"} 2`, `stiva_request_duration_seconds_count 2`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing metric %s", want)
		}
	}
}
