package s3api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// newSubresourceRouter builds a router over a temp engine holding one bucket
// with one object.
func newSubresourceRouter(t *testing.T) (*Router, storage.Engine) {
	t.Helper()
	eng, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	if err := eng.CreateBucket("probe-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if _, err := eng.PutObject(context.Background(), "probe-bucket", "keep.txt",
		strings.NewReader("data"), 4, "text/plain"); err != nil {
		t.Fatalf("put object: %v", err)
	}

	rt := NewRouter(RouterOptions{Engine: eng, Creds: auth.NewCredentials("ak", "sk"), Region: "us-east-1"})
	t.Cleanup(rt.Close)
	return rt, eng
}

// TestUnhandledObjectSubresourceDoesNotDelete is the regression test for the
// most dangerous bug found in the audit: DELETE /<bucket>/<key>?tagging fell
// through to handleDeleteObject and destroyed the object. Any S3 client calling
// the routine no-op DeleteObjectTagging would silently lose data.
func TestUnhandledObjectSubresourceDoesNotDelete(t *testing.T) {
	for _, sub := range []string{"tagging", "acl", "retention", "legal-hold", "torrent"} {
		t.Run(sub, func(t *testing.T) {
			rt, eng := newSubresourceRouter(t)

			req := httptest.NewRequest(http.MethodDelete, "/probe-bucket/keep.txt?"+sub, nil)
			w := httptest.NewRecorder()
			rt.handleObjectOps(w, req, "probe-bucket", "keep.txt")

			if w.Code != http.StatusNotImplemented {
				t.Errorf("DELETE ?%s: status = %d, want %d", sub, w.Code, http.StatusNotImplemented)
			}
			if _, err := eng.HeadObject(context.Background(), "probe-bucket", "keep.txt", ""); err != nil {
				t.Fatalf("DELETE ?%s DESTROYED THE OBJECT: %v", sub, err)
			}
		})
	}
}

// TestUnhandledBucketSubresourceDoesNotDelete covers the same fallthrough at
// bucket level: DELETE /<bucket>?tagging routed to handleDeleteBucket.
func TestUnhandledBucketSubresourceDoesNotDelete(t *testing.T) {
	for _, sub := range []string{"tagging", "policy", "website", "replication", "encryption"} {
		t.Run(sub, func(t *testing.T) {
			rt, eng := newSubresourceRouter(t)

			req := httptest.NewRequest(http.MethodDelete, "/probe-bucket?"+sub, nil)
			w := httptest.NewRecorder()
			rt.handleBucketOps(w, req, "probe-bucket")

			if w.Code != http.StatusNotImplemented {
				t.Errorf("DELETE ?%s: status = %d, want %d", sub, w.Code, http.StatusNotImplemented)
			}
			exists, err := eng.BucketExists("probe-bucket")
			if err != nil || !exists {
				t.Fatalf("DELETE ?%s DESTROYED THE BUCKET (exists=%v err=%v)", sub, exists, err)
			}
		})
	}
}

// TestUnhandledSubresourceDoesNotCreate covers the PUT side, where ?acl
// previously routed to CreateBucket and returned a misleading 409.
func TestUnhandledSubresourceDoesNotCreate(t *testing.T) {
	rt, _ := newSubresourceRouter(t)

	for _, sub := range []string{"acl", "policy", "website", "notification", "tagging"} {
		req := httptest.NewRequest(http.MethodPut, "/probe-bucket?"+sub, strings.NewReader(""))
		w := httptest.NewRecorder()
		rt.handleBucketOps(w, req, "probe-bucket")

		if w.Code != http.StatusNotImplemented {
			t.Errorf("PUT ?%s: status = %d, want %d", sub, w.Code, http.StatusNotImplemented)
		}
	}
}

// TestUnhandledSubresourceDoesNotListObjects covers GET, where an unknown
// subresource previously fell through to ListObjectsV2 and returned a bucket
// listing in response to e.g. GetBucketTagging.
func TestUnhandledSubresourceDoesNotListObjects(t *testing.T) {
	rt, _ := newSubresourceRouter(t)

	for _, sub := range []string{"tagging", "policy", "website", "analytics", "inventory"} {
		req := httptest.NewRequest(http.MethodGet, "/probe-bucket?"+sub, nil)
		w := httptest.NewRecorder()
		rt.handleBucketOps(w, req, "probe-bucket")

		if w.Code != http.StatusNotImplemented {
			t.Errorf("GET ?%s: status = %d, want %d", sub, w.Code, http.StatusNotImplemented)
		}
	}
}
