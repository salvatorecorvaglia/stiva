package storage

import (
	"context"
	bolt "go.etcd.io/bbolt"
	"io"
	"strings"
	"testing"
	"time"
)

type auditGate struct {
	ready  chan struct{}
	resume chan struct{}
	r      io.Reader
	once   bool
}

func (g *auditGate) Read(p []byte) (int, error) {
	if !g.once {
		g.once = true
		close(g.ready)
		<-g.resume
	}
	return g.r.Read(p)
}
func TestAuditVersioningDuringPut(t *testing.T) {
	e := setupTestEngine(t)
	e.CreateBucket("audit-bucket")
	g := &auditGate{ready: make(chan struct{}), resume: make(chan struct{}), r: strings.NewReader("slow")}
	done := make(chan error, 1)
	go func() {
		_, err := e.PutObject(context.Background(), "audit-bucket", "key", g, 4, "application/octet-stream")
		done <- err
	}()
	<-g.ready
	e.SetBucketVersioning("audit-bucket", "Enabled")
	first, err := e.PutObject(context.Background(), "audit-bucket", "key", strings.NewReader("new"), 3, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	close(g.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := e.HeadObject(context.Background(), "audit-bucket", "key", first.VersionID); err != nil {
		t.Fatalf("committed version lost after slow PUT: %v", err)
	}
}
func TestAuditDeleteUnpromotedNull(t *testing.T) {
	e := setupTestEngine(t)
	e.CreateBucket("audit-bucket")
	e.PutObject(context.Background(), "audit-bucket", "key", strings.NewReader("old"), 3, "application/octet-stream")
	e.SetBucketVersioning("audit-bucket", "Enabled")
	if _, _, err := e.DeleteObject("audit-bucket", "key", "null"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HeadObject(context.Background(), "audit-bucket", "key", ""); err == nil {
		t.Fatal("DELETE null reported success but object still exists")
	}
}
func TestAuditNoncurrentRetention(t *testing.T) {
	e := setupTestEngine(t)
	e.CreateBucket("audit-bucket")
	e.SetBucketVersioning("audit-bucket", "Enabled")
	first, err := e.PutObject(context.Background(), "audit-bucket", "key", strings.NewReader("old"), 3, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	first.LastModified = time.Now().Add(-60 * 24 * time.Hour)
	if err := e.metadata.PutObjectMetaRaw(first); err != nil {
		t.Fatal(err)
	}
	e.PutObject(context.Background(), "audit-bucket", "key", strings.NewReader("new"), 3, "application/octet-stream")
	lc := &LifecycleConfiguration{Rules: []LifecycleRule{{ID: "retain", Status: "Enabled", NoncurrentVersionExpiration: &NoncurrentVersionExpiration{NoncurrentDays: 30}}}}
	if err := e.PutBucketLifecycle("audit-bucket", lc); err != nil {
		t.Fatal(err)
	}
	if err := e.CleanExpiredObjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HeadObject(context.Background(), "audit-bucket", "key", first.VersionID); err != nil {
		t.Fatalf("version made noncurrent just now was already deleted: %v", err)
	}
}

func TestLegacyNoncurrentRetentionUsesSuccessor(t *testing.T) {
	e := setupTestEngine(t)
	if err := e.CreateBucket("legacy-retention"); err != nil {
		t.Fatal(err)
	}
	if err := e.SetBucketVersioning("legacy-retention", "Enabled"); err != nil {
		t.Fatal(err)
	}
	old, err := e.PutObject(context.Background(), "legacy-retention", "key", strings.NewReader("old"), 3, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	old.LastModified = time.Now().Add(-90 * 24 * time.Hour)
	if err := e.metadata.PutObjectMetaRaw(old); err != nil {
		t.Fatal(err)
	}
	newer, err := e.PutObject(context.Background(), "legacy-retention", "key", strings.NewReader("new"), 3, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	old.IsLatest = false
	old.NoncurrentSince = time.Time{}
	if err := e.metadata.PutObjectMetaRaw(old); err != nil {
		t.Fatal(err)
	}
	newer.LastModified = time.Now().Add(-40 * 24 * time.Hour)
	if err := e.metadata.PutObjectMetaRaw(newer); err != nil {
		t.Fatal(err)
	}
	lc := &LifecycleConfiguration{Rules: []LifecycleRule{{ID: "retention", Status: "Enabled", NoncurrentVersionExpiration: &NoncurrentVersionExpiration{NoncurrentDays: 30}}}}
	if err := e.PutBucketLifecycle("legacy-retention", lc); err != nil {
		t.Fatal(err)
	}
	if err := e.CleanExpiredObjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HeadObject(context.Background(), "legacy-retention", "key", old.VersionID); err == nil {
		t.Fatal("legacy noncurrent version should expire using successor age")
	}
	if _, err := e.HeadObject(context.Background(), "legacy-retention", "key", newer.VersionID); err != nil {
		t.Fatal(err)
	}
}

func TestExpirationRetainsReplacementAndPromotedVersions(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("expiration-races"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	old, err := engine.PutObject(ctx, "expiration-races", "plain", strings.NewReader("old"), 3, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := engine.PutObject(ctx, "expiration-races", "plain", strings.NewReader("new"), 3, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.expireCurrentVersion("expiration-races", "plain", old.VersionID, old.LastModified); err != nil {
		t.Fatal(err)
	}
	current, err := engine.HeadObject(ctx, "expiration-races", "plain", "")
	if err != nil || current.ETag != replacement.ETag {
		t.Fatalf("fresh replacement was expired: %v", err)
	}
	if err := engine.SetBucketVersioning("expiration-races", "Enabled"); err != nil {
		t.Fatal(err)
	}
	first, err := engine.PutObject(ctx, "expiration-races", "versioned", strings.NewReader("first"), 5, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.PutObject(ctx, "expiration-races", "versioned", strings.NewReader("second"), 6, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.DeleteObject("expiration-races", "versioned", second.VersionID); err != nil {
		t.Fatal(err)
	}
	if err := engine.expireNoncurrentVersion("expiration-races", "versioned", first.VersionID, time.Now().Add(48*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	current, err = engine.HeadObject(ctx, "expiration-races", "versioned", "")
	if err != nil || current.VersionID != first.VersionID {
		t.Fatalf("promoted current version was expired: %v", err)
	}
}

func TestLifecycleReportsCorruptMetadata(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("corrupt-lifecycle"); err != nil {
		t.Fatal(err)
	}
	if err := engine.PutBucketLifecycle("corrupt-lifecycle", &LifecycleConfiguration{Rules: []LifecycleRule{{ID: "expire", Status: "Enabled", Expiration: &LifecycleExpiration{Days: 1}}}}); err != nil {
		t.Fatal(err)
	}
	db, release, err := engine.metadata.acquireBucketDB("corrupt-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(objectsBucket).Put(objectMetaKey("corrupt-lifecycle", "key"), []byte("{"))
	})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CleanExpiredObjects(context.Background()); err == nil {
		t.Fatal("corrupt metadata was silently ignored")
	}
}
