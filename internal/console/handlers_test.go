package console

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// These endpoints had no coverage at all, which is also where the raw-error
// leaks and missing body limits lived.

func newHandlerFixture(t *testing.T) (*Handler, *storage.FilesystemEngine, string) {
	t.Helper()
	engine, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	h := NewHandler(Options{
		Engine:         engine,
		Creds:          auth.NewCredentials("access", "secret"),
		S3Port:         9000,
		Region:         "us-east-1",
		LoginRateLimit: 1000,
		APIRateLimit:   10000,
	})
	token, err := GenerateToken("access")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return h, engine, token
}

func do(t *testing.T, h *Handler, method, target, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBucketLifecycleEndpoints(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("life"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// Absent configuration reports not-found rather than an empty success.
	if w := do(t, h, http.MethodGet, "/api/buckets/life/lifecycle", token, nil); w.Code != http.StatusNotFound {
		t.Errorf("GET lifecycle (unset) = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}

	cfg, _ := json.Marshal(map[string]interface{}{
		"rules": []map[string]interface{}{
			{"id": "expire", "status": "Enabled", "filter": map[string]string{"prefix": "tmp/"},
				"expiration": map[string]int{"days": 7}},
		},
	})
	if w := do(t, h, http.MethodPost, "/api/buckets/life/lifecycle", token, cfg); w.Code != http.StatusOK {
		t.Fatalf("POST lifecycle = %d (body: %s)", w.Code, w.Body.String())
	}

	w := do(t, h, http.MethodGet, "/api/buckets/life/lifecycle", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET lifecycle = %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "expire") {
		t.Errorf("lifecycle response missing the stored rule: %s", w.Body.String())
	}

	if w := do(t, h, http.MethodDelete, "/api/buckets/life/lifecycle", token, nil); w.Code != http.StatusOK {
		t.Errorf("DELETE lifecycle = %d (body: %s)", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/api/buckets/life/lifecycle", token, nil); w.Code != http.StatusNotFound {
		t.Errorf("GET lifecycle after delete = %d, want 404", w.Code)
	}
}

func TestBucketPublicToggle(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("pub"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if w := do(t, h, http.MethodPost, "/api/buckets/pub/public?public=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("set public = %d (body: %s)", w.Code, w.Body.String())
	}
	w := do(t, h, http.MethodGet, "/api/buckets/pub/public", token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"public":true`) {
		t.Errorf("get public = %d %s, want public:true", w.Code, w.Body.String())
	}

	if w := do(t, h, http.MethodPost, "/api/buckets/pub/public?public=false", token, nil); w.Code != http.StatusOK {
		t.Fatalf("unset public = %d", w.Code)
	}
	w = do(t, h, http.MethodGet, "/api/buckets/pub/public", token, nil)
	if !strings.Contains(w.Body.String(), `"public":false`) {
		t.Errorf("get public = %s, want public:false", w.Body.String())
	}
}

// TestBucketPublicToggleRejectsGarbage covers a control that used to fail
// silently: anything other than "true" meant false and still returned 200, so a
// typo in a call meant to publish a bucket reported success while doing the
// opposite.
func TestBucketPublicToggleRejectsGarbage(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("pub"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	for _, v := range []string{"", "maybe", "tru", "2"} {
		w := do(t, h, http.MethodPost, "/api/buckets/pub/public?public="+v, token, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("public=%q = %d, want 400 (body: %s)", v, w.Code, w.Body.String())
		}
	}
}

func TestObjectUploadDownloadDeleteRoundTrip(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("objs"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "hello.txt")
	_, _ = fw.Write([]byte("hello console"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/buckets/objs/objects/upload?key=docs/hello.txt", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d (body: %s)", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodGet, "/api/buckets/objs/objects/download?key=docs/hello.txt", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("download = %d (body: %s)", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello console" {
		t.Errorf("download body = %q", w.Body.String())
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("download X-Content-Type-Options = %q, want nosniff", got)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("download Content-Disposition = %q, want attachment", cd)
	}

	w = do(t, h, http.MethodGet, "/api/buckets/objs/objects?prefix=docs/", token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "docs/hello.txt") {
		t.Errorf("list = %d %s", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodDelete, "/api/buckets/objs/objects?key=docs/hello.txt", token, nil)
	if w.Code != http.StatusOK {
		t.Errorf("delete = %d (body: %s)", w.Code, w.Body.String())
	}
	if _, _, err := engine.GetObject(context.Background(), "objs", "docs/hello.txt", ""); err == nil {
		t.Error("object still present after delete")
	}
}

func TestBucketCreateListDelete(t *testing.T) {
	h, _, token := newHandlerFixture(t)

	body, _ := json.Marshal(map[string]string{"name": "made-by-console"})
	if w := do(t, h, http.MethodPost, "/api/buckets", token, body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (body: %s)", w.Code, w.Body.String())
	}

	w := do(t, h, http.MethodGet, "/api/buckets", token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "made-by-console") {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}

	if w := do(t, h, http.MethodDelete, "/api/buckets/made-by-console", token, nil); w.Code != http.StatusOK {
		t.Errorf("delete = %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestErrorsDoNotLeakFilesystemPaths covers the console's habit of echoing
// engine error text, which carries the data directory's real paths.
func TestErrorsDoNotLeakFilesystemPaths(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("exists"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	cases := []struct {
		method, target string
		body           []byte
	}{
		{http.MethodPost, "/api/buckets", mustJSON(map[string]string{"name": "exists"})},
		{http.MethodDelete, "/api/buckets/never-created", nil},
		{http.MethodGet, "/api/buckets/never-created/objects", nil},
		{http.MethodDelete, "/api/buckets/never-created/objects?key=x", nil},
		{http.MethodGet, "/api/buckets/never-created/lifecycle", nil},
	}
	for _, c := range cases {
		w := do(t, h, c.method, c.target, token, c.body)
		body := w.Body.String()
		for _, leak := range []string{"/var/folders", "/tmp/", ".db", "mkdir", "no such file", "TempDir"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s %s leaks %q: %s", c.method, c.target, leak, body)
			}
		}
	}
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestRoutingMethodAndPathHandling covers what the hand-rolled dispatcher used
// to do by hand: a known path with the wrong method is 405, and an unknown
// /api/ path is a JSON 404 rather than the SPA's index.html.
func TestRoutingMethodAndPathHandling(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("bkt"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if w := do(t, h, http.MethodPut, "/api/buckets/bkt/objects", token, nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT objects = %d, want 405", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/login", token, nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET login = %d, want 405", w.Code)
	}

	w := do(t, h, http.MethodGet, "/api/buckets/bkt/nonsense", token, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown api path = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "<html") || strings.Contains(w.Body.String(), "<!doctype") {
		t.Errorf("unknown api path fell through to the SPA fallback: %s", w.Body.String())
	}

	if w := do(t, h, http.MethodGet, "/api/buckets/UPPERCASE/objects", token, nil); w.Code != http.StatusBadRequest {
		t.Errorf("invalid bucket name = %d, want 400", w.Code)
	}
}

// TestAPIRequiresAuthentication pins that every bucket route sits behind the
// session check.
func TestAPIRequiresAuthentication(t *testing.T) {
	h, engine, _ := newHandlerFixture(t)
	if err := engine.CreateBucket("bkt"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	for _, target := range []string{
		"/api/config",
		"/api/buckets",
		"/api/buckets/bkt/objects",
		"/api/buckets/bkt/public",
		"/api/buckets/bkt/lifecycle",
		"/api/buckets/bkt/objects/download?key=x",
	} {
		if w := do(t, h, http.MethodGet, target, "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", target, w.Code)
		}
	}
}

func TestAbortMultipartUpload(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("aborts"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	up, err := engine.CreateMultipartUpload("aborts", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	body := mustJSON(map[string]string{"uploadId": up.UploadID, "key": "big.bin"})
	if w := do(t, h, http.MethodPost, "/api/buckets/aborts/multipart/abort", token, body); w.Code != http.StatusOK {
		t.Fatalf("abort = %d (body: %s)", w.Code, w.Body.String())
	}

	// The upload is gone, so a second abort is rejected rather than silently OK.
	w := do(t, h, http.MethodPost, "/api/buckets/aborts/multipart/abort", token, body)
	if w.Code == http.StatusOK {
		t.Error("aborting an unknown upload reported success")
	}
	if strings.Contains(w.Body.String(), "/var/folders") || strings.Contains(w.Body.String(), "/tmp/") {
		t.Errorf("abort error leaks a filesystem path: %s", w.Body.String())
	}
}

// TestMetricsRequiresAuthorization covers the /metrics endpoint, which sits
// outside the authMiddleware and does its own check against either a static
// token or a console session.
func TestMetricsRequiresAuthorization(t *testing.T) {
	engine, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer engine.Close()

	h := NewHandler(Options{
		Engine:         engine,
		Creds:          auth.NewCredentials("access", "secret"),
		S3Port:         9000,
		Region:         "us-east-1",
		LoginRateLimit: 1000,
		APIRateLimit:   10000,
		MetricsToken:   "s3cr3t-metrics-token",
	})
	token, err := GenerateToken("access")
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	if w := do(t, h, http.MethodGet, "/metrics", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("no credentials = %d, want 401", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/metrics", "wrong-token", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", w.Code)
	}

	w := do(t, h, http.MethodGet, "/metrics", "s3cr3t-metrics-token", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics token = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stiva_requests_total") {
		t.Errorf("metrics body missing counters: %s", w.Body.String())
	}

	if w := do(t, h, http.MethodGet, "/metrics", token, nil); w.Code != http.StatusOK {
		t.Errorf("console session = %d, want 200", w.Code)
	}
}

// TestConsoleOriginCheck pins which cross-origin callers the console answers.
// Note that any localhost origin is accepted regardless of port or scheme,
// which is deliberate for local development but worth seeing stated.
func TestConsoleOriginCheck(t *testing.T) {
	tests := []struct {
		origin, host string
		want         bool
	}{
		{"https://console.example.com", "console.example.com", true},
		{"https://CONSOLE.example.com", "console.example.com", true},
		{"https://evil.example.com", "console.example.com", false},
		{"https://console.example.com.evil.com", "console.example.com", false},
		{"http://localhost:5173", "console.example.com", true},
		{"http://127.0.0.1:8080", "console.example.com", true},
		{"", "console.example.com", false},
		{"://bad", "console.example.com", false},
	}
	for _, tc := range tests {
		if got := isValidConsoleOrigin(tc.origin, tc.host); got != tc.want {
			t.Errorf("isValidConsoleOrigin(%q, %q) = %v, want %v", tc.origin, tc.host, got, tc.want)
		}
	}
}

// TestForeignOriginIsRejected checks the same rule through the handler.
func TestForeignOriginIsRejected(t *testing.T) {
	h, _, token := newHandlerFixture(t)

	r := httptest.NewRequest(http.MethodGet, "/api/buckets", nil)
	r.Host = "console.example.com"
	r.Header.Set("Origin", "https://attacker.example")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("foreign origin = %d, want 403", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("foreign origin was echoed back in Access-Control-Allow-Origin")
	}
}
