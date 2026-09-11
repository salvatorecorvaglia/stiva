package s3api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func newHardeningRouter(t *testing.T) (*Router, storage.Engine, *auth.Credentials) {
	t.Helper()
	eng, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	creds := auth.NewCredentials("ak", "sk")
	rt := NewRouter(RouterOptions{Engine: eng, Creds: creds, Region: "us-east-1"})
	t.Cleanup(rt.Close)
	return rt, eng, creds
}

func seedObject(t *testing.T, eng storage.Engine, bucket, key, body, contentType string) {
	t.Helper()
	if err := eng.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if _, err := eng.PutObject(context.Background(), bucket, key,
		strings.NewReader(body), int64(len(body)), contentType); err != nil {
		t.Fatalf("put object: %v", err)
	}
}

// TestPublicReadIgnoresResponseTypeOverride covers a stored-XSS vector on the
// S3 origin. The response-header overrides restate an object's Content-Type and
// Content-Disposition, and they were honoured on unauthenticated public-bucket
// reads — so any stored object could be served as arbitrary HTML from this
// origin by appending a query parameter. S3 honours them for signed requests.
func TestPublicReadIgnoresResponseTypeOverride(t *testing.T) {
	rt, eng, _ := newHardeningRouter(t)
	seedObject(t, eng, "pub", "payload.txt", "<script>alert(1)</script>", "text/plain")

	if err := eng.SetBucketPublic("pub", true); err != nil {
		t.Fatalf("set public: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/pub/payload.txt?response-content-type=text/html", nil)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q; an unsigned public read must not be able to override it to HTML", ct)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the object's stored text/plain", ct)
	}
}

// TestSignedReadHonoursResponseOverrides is the counterpart: the overrides must
// keep working for signed requests, which is what presigned download links use.
func TestSignedReadHonoursResponseOverrides(t *testing.T) {
	rt, eng, creds := newHardeningRouter(t)
	seedObject(t, eng, "priv", "doc.txt", "hello", "text/plain")

	// A value with no characters that the canonical-query encoding would
	// normalise, so this test stays about the override and not about signing.
	req := httptest.NewRequest(http.MethodGet, "/priv/doc.txt?response-content-type=text/html", nil)
	req.Host = "example.com"
	signPut(req, creds, "us-east-1", time.Now().UTC(), nil)

	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want the signed request's override to apply", ct)
	}
}

// TestObjectResponsesSetNosniff pins the header that stops a browser sniffing
// caller-supplied bytes into something executable. The console download path
// already set it; the S3 path did not.
func TestObjectResponsesSetNosniff(t *testing.T) {
	rt, eng, creds := newHardeningRouter(t)
	seedObject(t, eng, "sniff", "a.txt", "hello", "text/plain")

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/sniff/a.txt", nil)
			req.Host = "example.com"
			signPut(req, creds, "us-east-1", time.Now().UTC(), nil)

			w := httptest.NewRecorder()
			rt.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

// TestInternalErrorsAreNotEchoed covers three handlers that passed err.Error()
// into the response, contradicting the rule the rest of the S3 layer follows —
// engine errors carry filesystem paths.
func TestInternalErrorsAreNotEchoed(t *testing.T) {
	rt, _, creds := newHardeningRouter(t)

	// HeadBucket and GetBucketLocation on a bucket whose name is invalid make
	// the engine return an error rather than a plain "not found".
	for _, target := range []string{"/A_B/", "/A_B/?location"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "example.com"
		signPut(req, creds, "us-east-1", time.Now().UTC(), nil)

		w := httptest.NewRecorder()
		rt.ServeHTTP(w, req)

		body := w.Body.String()
		for _, leak := range []string{"/tmp/", "mkdir", ".db", "no such file"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s response leaks internal detail %q: %s", target, leak, body)
			}
		}
	}
}
