package s3api

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func newOpsRouter(t *testing.T) (*Router, storage.Engine) {
	t.Helper()
	eng, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	rt := NewRouter(RouterOptions{
		Engine: eng,
		Creds:  auth.NewCredentials("ak", "sk"),
		Region: "us-east-1",
	})
	t.Cleanup(rt.Close)
	return rt, eng
}

func putObjects(t *testing.T, eng storage.Engine, bucket string, keys ...string) {
	t.Helper()
	if err := eng.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	for _, k := range keys {
		if _, err := eng.PutObject(context.Background(), bucket, k,
			strings.NewReader("x"), 1, "application/octet-stream"); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
}

// TestDeleteObjectsBatch covers the newly added batch delete, the operation the
// AWS CLI issues for `s3 rm --recursive` and `s3 sync --delete`. Without it
// recursive deletion through standard tooling failed outright.
func TestDeleteObjectsBatch(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "batch", "a.txt", "b.txt", "c.txt")

	body := `<Delete>
		<Object><Key>a.txt</Key></Object>
		<Object><Key>b.txt</Key></Object>
	</Delete>`

	req := httptest.NewRequest(http.MethodPost, "/batch?delete", strings.NewReader(body))
	w := httptest.NewRecorder()
	rt.handleBucketOps(w, req, "batch")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var result DeleteObjectsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Deleted) != 2 {
		t.Errorf("deleted %d objects, want 2", len(result.Deleted))
	}
	if len(result.Errors) != 0 {
		t.Errorf("unexpected errors: %+v", result.Errors)
	}

	for _, gone := range []string{"a.txt", "b.txt"} {
		if _, err := eng.HeadObject(context.Background(), "batch", gone, ""); err == nil {
			t.Errorf("%s should have been deleted", gone)
		}
	}
	if _, err := eng.HeadObject(context.Background(), "batch", "c.txt", ""); err != nil {
		t.Errorf("c.txt was not requested for deletion but is gone: %v", err)
	}
}

func TestDeleteObjectsQuietMode(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "quiet", "a.txt")

	body := `<Delete><Quiet>true</Quiet><Object><Key>a.txt</Key></Object></Delete>`
	req := httptest.NewRequest(http.MethodPost, "/quiet?delete", strings.NewReader(body))
	w := httptest.NewRecorder()
	rt.handleBucketOps(w, req, "quiet")

	var result DeleteObjectsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Deleted) != 0 {
		t.Errorf("quiet mode should omit <Deleted> elements, got %d", len(result.Deleted))
	}
	if _, err := eng.HeadObject(context.Background(), "quiet", "a.txt", ""); err == nil {
		t.Error("a.txt should still be deleted on disk even in quiet mode")
	}
}

func TestDeleteObjectsNonexistentKeys(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "batch")

	body := `<Delete><Object><Key>ghost.txt</Key></Object></Delete>`
	req := httptest.NewRequest(http.MethodPost, "/batch?delete", strings.NewReader(body))
	w := httptest.NewRecorder()
	rt.handleBucketOps(w, req, "batch")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var result DeleteObjectsResult
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Deleted) != 1 || result.Deleted[0].Key != "ghost.txt" {
		t.Errorf("S3 specification mandates deleting nonexistent keys reports success, got: %+v", result)
	}
}

func TestListObjectsV1Pagination(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "v1bucket", "a", "b", "c", "d", "e")

	req := httptest.NewRequest(http.MethodGet, "/v1bucket?max-keys=2", nil)
	w := httptest.NewRecorder()
	rt.handleBucketOps(w, req, "v1bucket")

	var res ListBucketResultV1
	if err := xml.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode V1 list: %v", err)
	}
	if len(res.Contents) != 2 || !res.IsTruncated {
		t.Fatalf("page 1: len=%d isTruncated=%v", len(res.Contents), res.IsTruncated)
	}
	if res.NextMarker != "b" {
		t.Errorf("NextMarker = %q, want %q", res.NextMarker, "b")
	}

	// Fetch page 2 using marker=b
	req = httptest.NewRequest(http.MethodGet, "/v1bucket?max-keys=2&marker=b", nil)
	w = httptest.NewRecorder()
	rt.handleBucketOps(w, req, "v1bucket")

	var res2 ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &res2)
	if len(res2.Contents) != 2 || res2.Contents[0].Key != "c" || res2.Contents[1].Key != "d" {
		t.Errorf("page 2 items: %+v", res2.Contents)
	}

	// Fetch page 3
	req = httptest.NewRequest(http.MethodGet, "/v1bucket?max-keys=2&marker=d", nil)
	w = httptest.NewRecorder()
	rt.handleBucketOps(w, req, "v1bucket")

	var res3 ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &res3)
	if len(res3.Contents) != 1 || res3.IsTruncated {
		t.Errorf("page 3 items: len=%d isTruncated=%v", len(res3.Contents), res3.IsTruncated)
	}
}

func TestListObjectsV1CommonPrefixes(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "pref", "photos/1.jpg", "photos/2.jpg", "docs/a.txt", "root.txt")

	req := httptest.NewRequest(http.MethodGet, "/pref?delimiter=/", nil)
	w := httptest.NewRecorder()
	rt.handleBucketOps(w, req, "pref")

	var res ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &res)

	if len(res.Contents) != 1 || res.Contents[0].Key != "root.txt" {
		t.Errorf("contents = %+v, want [root.txt]", res.Contents)
	}
	if len(res.CommonPrefixes) != 2 {
		t.Fatalf("common prefixes len = %d, want 2", len(res.CommonPrefixes))
	}
	prefixes := res.CommonPrefixes[0].Prefix + "," + res.CommonPrefixes[1].Prefix
	if !strings.Contains(prefixes, "photos/") || !strings.Contains(prefixes, "docs/") {
		t.Errorf("prefixes = %q", prefixes)
	}
}

func formatETagHeader(etag string) string {
	if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) {
		return etag
	}
	return fmt.Sprintf(`"%s"`, etag)
}

func TestGetObjectIfNoneMatch304(t *testing.T) {
	rt, eng := newOpsRouter(t)
	if err := eng.CreateBucket("cond"); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := eng.PutObject(context.Background(), "cond", "k.txt",
		strings.NewReader("hello"), 5, "text/plain")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// 1. Matching ETag must return 304 Not Modified with empty body.
	req := httptest.NewRequest(http.MethodGet, "/cond/k.txt", nil)
	req.Header.Set("If-None-Match", formatETagHeader(info.ETag))
	w := httptest.NewRecorder()
	rt.handleObjectOps(w, req, "cond", "k.txt")

	if w.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match match: status = %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 response body must be empty, got %d bytes", w.Body.Len())
	}

	// 2. Mismatched ETag must return 200 OK with full body.
	req = httptest.NewRequest(http.MethodGet, "/cond/k.txt", nil)
	req.Header.Set("If-None-Match", `"wrong-etag"`)
	w = httptest.NewRecorder()
	rt.handleObjectOps(w, req, "cond", "k.txt")

	if w.Code != http.StatusOK {
		t.Fatalf("If-None-Match mismatch: status = %d, want 200", w.Code)
	}
	if w.Body.String() != "hello" {
		t.Errorf("body = %q, want hello", w.Body.String())
	}
}

// TestGetObjectResponseHeaderOverrides covers the response-content-disposition
// (and friends) query parameters real S3 honors on GetObject, commonly used
// on presigned URLs so a link opened directly in a browser downloads instead
// of rendering inline per the object's stored Content-Type.
func TestGetObjectResponseHeaderOverrides(t *testing.T) {
	rt, eng := newOpsRouter(t)
	if err := eng.CreateBucket("overrides"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := eng.PutObject(context.Background(), "overrides", "report.txt",
		strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		`/overrides/report.txt?response-content-disposition=attachment%3B+filename%3D%22report.txt%22&response-content-type=application%2Foctet-stream`, nil)
	w := httptest.NewRecorder()
	rt.handleObjectOps(w, req, "overrides", "report.txt")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="report.txt"` {
		t.Errorf("Content-Disposition = %q, want %q", got, `attachment; filename="report.txt"`)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want overridden value application/octet-stream", got)
	}
}

func TestHeadObjectIfMatchPreconditionFailed(t *testing.T) {
	rt, eng := newOpsRouter(t)
	if err := eng.CreateBucket("cond"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := eng.PutObject(context.Background(), "cond", "k.txt",
		strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}

	req := httptest.NewRequest(http.MethodHead, "/cond/k.txt", nil)
	req.Header.Set("If-Match", `"stale-etag"`)
	w := httptest.NewRecorder()
	rt.handleObjectOps(w, req, "cond", "k.txt")

	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("If-Match mismatch on HEAD: status = %d, want 412", w.Code)
	}
}
