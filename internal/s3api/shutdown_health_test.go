package s3api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// TestRouterShutdownRespectsDeadline is the regression test for Router.Close
// waiting on in-flight requests with no bound: one slow download held server
// shutdown open indefinitely, however short the shutdown timeout.
func TestRouterShutdownRespectsDeadline(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "logged", "obj.txt")
	putObjects(t, eng, "logs")
	if err := eng.PutBucketLogging("logged", &storage.BucketLoggingStatus{
		LoggingEnabled: &storage.LoggingEnabled{TargetBucket: "logs", TargetPrefix: "access/"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetBucketPublic("logged", true); err != nil {
		t.Fatal(err)
	}

	// A request that never finishes.
	rt.activeReqs.add()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := rt.Shutdown(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %v with a 100ms deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want the missed deadline reported", err)
	}

	// The straggler finishes after shutdown gave up on it, and its access log
	// entry must be dropped quietly — the log channel used to be closed at
	// this point, so this send panicked.
	rt.activeReqs.done()
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logged/obj.txt", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("late request status %d", w.Code)
	}
}

// TestHealthEndpoint covers the unauthenticated liveness probe used by the
// container healthcheck. "_health" can never be a bucket name (S3 bucket
// names allow no underscore), so it cannot shadow a real bucket.
func TestHealthEndpoint(t *testing.T) {
	rt, _ := newOpsRouter(t)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, httptest.NewRequest(method, "/_health", nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s /_health = %d, want 200 without credentials", method, w.Code)
		}
	}

	// Anything below it is not the probe, and still needs authentication.
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/_health/x", nil))
	if w.Code == http.StatusOK {
		t.Error("/_health/x was served without authentication")
	}

	// With virtual-host routing, /_health is an ordinary object key in the
	// named bucket, not the probe.
	vrt := NewRouter(RouterOptions{Engine: rt.engine, Creds: rt.verifier.Creds(), Region: "us-east-1", Domain: "s3.example.com"})
	defer vrt.Close()
	req := httptest.NewRequest(http.MethodGet, "/_health", nil)
	req.Host = "mybucket.s3.example.com"
	w = httptest.NewRecorder()
	vrt.ServeHTTP(w, req)
	if w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("virtual-host GET /_health = %d %s; want an authenticated object request", w.Code, w.Body.String())
	}
}

// TestAccessLogLineEscapesCallerInput is the regression test for access log
// injection: the key, User-Agent, Referer and the access key named in the
// Authorization header went into the log verbatim, so a newline forged whole
// log entries and a quote or space shifted the fields after it.
func TestAccessLogLineEscapesCallerInput(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	req.Header.Set("User-Agent", "evil\"agent\nstiva bucket [01/Jan/2026:00:00:00 +0000] 1.2.3.4 forged")
	req.Header.Set("Referer", "https://x/\"quoted\"")

	line := formatAccessLogLine(req, "bucket", "a key\nwith \"quotes\"", "10.0.0.1", "AK ID\nforged", "0123456789ABCDEF",
		http.StatusNotFound, 12, 3*time.Millisecond, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("log entry spans several lines:\n%s", line)
	}

	// Split outside quotes: every entry must have the same number of fields
	// however hostile its input.
	var fields []string
	var cur strings.Builder
	inQuotes := false
	for _, c := range strings.TrimSuffix(line, "\n") {
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case c == ' ' && !inQuotes:
			fields = append(fields, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(c)
	}
	fields = append(fields, cur.String())
	// The timestamp is "[02/Jan/2006:15:04:05 -0700]", which splits in two.
	if len(fields) != 19 {
		t.Fatalf("got %d fields, want 19:\n%q", len(fields), fields)
	}
	if fields[8] != "a%20key%0Awith%20%22quotes%22" {
		t.Errorf("key field = %q, want it percent-encoded", fields[8])
	}
	if fields[11] != "Not%20Found" {
		t.Errorf("error field = %q, want a single token", fields[11])
	}
	if !strings.Contains(line, `"evil\x22agent\x0Astiva`) {
		t.Errorf("User-Agent not escaped inside its quotes:\n%s", line)
	}
}

// TestPublicBucketAnonymousReadIsLatestObjectOnly is the regression test for
// anonymous access reaching more than "read the current object". Any GET or
// HEAD of a key in a public bucket skipped authentication, including
// ?uploadId= (ListParts of in-progress uploads) and ?versionId= (every old
// and deleted version, which in S3 needs s3:GetObjectVersion).
func TestPublicBucketAnonymousReadIsLatestObjectOnly(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "pub", "doc.txt")
	if err := eng.SetBucketPublic("pub", true); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetBucketVersioning("pub", "Enabled"); err != nil {
		t.Fatal(err)
	}
	old, err := eng.PutObject(context.Background(), "pub", "doc.txt", strings.NewReader("v1"), 2, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.DeleteObject("pub", "doc.txt", ""); err != nil { // deleted: only old versions remain
		t.Fatal(err)
	}
	if _, err := eng.PutObject(context.Background(), "pub", "live.txt", strings.NewReader("live"), 4, "text/plain"); err != nil {
		t.Fatal(err)
	}
	up, err := eng.CreateMultipartUpload(context.Background(), "pub", "upload.bin", "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}

	serve := func(method, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, httptest.NewRequest(method, target, nil))
		return w
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if w := serve(method, "/pub/live.txt"); w.Code != http.StatusOK {
			t.Errorf("anonymous %s of a public object = %d, want 200", method, w.Code)
		}
	}
	for _, target := range []string{
		"/pub/doc.txt?versionId=" + old.VersionID,
		"/pub/upload.bin?uploadId=" + up.UploadID,
	} {
		if w := serve(http.MethodGet, target); w.Code != http.StatusForbidden {
			t.Errorf("anonymous GET %s = %d, want 403", target, w.Code)
		}
	}
}

// TestRequestIDIsPerRequestAndShared is the regression test for request IDs
// that correlated nothing: the x-amz-request-id header was the constant
// "stiva" on every response while error bodies carried a random UUID of
// their own, so a client's error report could not be matched to a log line.
func TestRequestIDIsPerRequestAndShared(t *testing.T) {
	rt, _ := newOpsRouter(t)

	serve := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/no-such-bucket/key", nil)) // unsigned: an error
		return w
	}
	a, b := serve(), serve()

	idA := a.Header().Get("x-amz-request-id")
	if idA == "" || idA == "stiva" {
		t.Fatalf("x-amz-request-id = %q, want a per-request ID", idA)
	}
	if idA == b.Header().Get("x-amz-request-id") {
		t.Errorf("two requests got the same request ID %q", idA)
	}
	if !strings.Contains(a.Body.String(), "<RequestId>"+idA+"</RequestId>") {
		t.Errorf("error body does not carry the header's request ID %q:\n%s", idA, a.Body.String())
	}

	line := formatAccessLogLine(httptest.NewRequest(http.MethodGet, "/b/k", nil), "b", "k", "10.0.0.1", "", idA,
		http.StatusOK, 0, 0, time.Now())
	if !strings.Contains(line, " "+idA+" ") {
		t.Errorf("access log line does not carry the request ID:\n%s", line)
	}
}
