package console

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func TestConsoleEndpoints(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "console-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	engine, err := storage.NewFilesystemEngine(tempDir, nil, "")
	if err != nil {
		t.Fatalf("failed to initialize storage engine: %v", err)
	}
	defer engine.Close()

	creds := auth.NewCredentials("access", "secret")
	handler := NewHandler(Options{Engine: engine, Creds: creds, S3Port: 9000, Region: "us-east-1", LoginRateLimit: 100, APIRateLimit: 1000})

	err = engine.CreateBucket("test-bucket")
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	token, err := GenerateToken("access")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// 1. Test Multipart Initiate
	initiateBody, _ := json.Marshal(map[string]string{
		"key":         "large-file.bin",
		"contentType": "application/octet-stream",
	})
	req := httptest.NewRequest("POST", "/api/buckets/test-bucket/multipart/initiate", bytes.NewReader(initiateBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var initResp struct {
		UploadID string `json:"uploadId"`
		Key      string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("failed to parse initiate response: %v", err)
	}

	if initResp.UploadID == "" || initResp.Key != "large-file.bin" {
		t.Errorf("invalid initiate response: %+v", initResp)
	}

	// 2. Test Multipart Upload Part
	bodyBuf := &bytes.Buffer{}
	writer := multipart.NewWriter(bodyBuf)
	writer.WriteField("uploadId", initResp.UploadID)
	writer.WriteField("key", "large-file.bin")
	writer.WriteField("partNumber", "1")
	partWriter, _ := writer.CreateFormFile("file", "part1.bin")
	partWriter.Write([]byte("chunk-data-1"))
	writer.Close()

	req = httptest.NewRequest("POST", "/api/buckets/test-bucket/multipart/upload-part", bodyBuf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var partResp struct {
		ETag       string `json:"etag"`
		PartNumber int    `json:"partNumber"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &partResp); err != nil {
		t.Fatalf("failed to parse upload-part response: %v", err)
	}

	if partResp.PartNumber != 1 || partResp.ETag == "" {
		t.Errorf("invalid part response: %+v", partResp)
	}

	// 3. Test Multipart Complete
	completeBody, _ := json.Marshal(map[string]interface{}{
		"uploadId": initResp.UploadID,
		"key":      "large-file.bin",
		"parts": []map[string]interface{}{
			{"partNumber": 1, "etag": partResp.ETag},
		},
	})

	req = httptest.NewRequest("POST", "/api/buckets/test-bucket/multipart/complete", bytes.NewReader(completeBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	// 4. Test List Objects with pagination and search
	req = httptest.NewRequest("GET", "/api/buckets/test-bucket/objects?prefix=&delimiter=/&maxKeys=5", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var listResp struct {
		Items []struct {
			Key string `json:"key"`
		} `json:"items"`
		IsTruncated           bool   `json:"isTruncated"`
		NextContinuationToken string `json:"nextContinuationToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to parse list response: %v", err)
	}

	if len(listResp.Items) != 1 || listResp.Items[0].Key != "large-file.bin" {
		t.Errorf("expected 1 item with key 'large-file.bin', got items: %+v", listResp.Items)
	}

	// Test Search
	req = httptest.NewRequest("GET", "/api/buckets/test-bucket/objects?searchPrefix=large", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var searchResp struct {
		Items []struct {
			Key string `json:"key"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &searchResp)
	if len(searchResp.Items) != 1 || searchResp.Items[0].Key != "large-file.bin" {
		t.Errorf("search failed: expected large-file.bin, got %+v", searchResp.Items)
	}

	// Test Search with no matches
	req = httptest.NewRequest("GET", "/api/buckets/test-bucket/objects?searchPrefix=nonexistent", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	json.Unmarshal(w.Body.Bytes(), &searchResp)
	if len(searchResp.Items) != 0 {
		t.Errorf("expected 0 matches, got %d", len(searchResp.Items))
	}

	// 5. Test Presign Object
	req = httptest.NewRequest("GET", "/api/buckets/test-bucket/objects/presign?key=large-file.bin&expires=3600", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 for presign, got %d, body: %s", w.Code, w.Body.String())
	}

	var presignResp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &presignResp); err != nil {
		t.Fatalf("failed to parse presign response: %v", err)
	}

	if presignResp.URL == "" {
		t.Errorf("expected presigned URL, got empty")
	}
}

// TestPresignObjectDownloadOverride covers the share-link "force download"
// option: a share link is opened outside the console origin, so there's no
// <a download> to fall back on, and without response-content-disposition the
// object would just render inline per its stored Content-Type in the
// recipient's browser.
func TestPresignObjectDownloadOverride(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "console-presign-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	engine, err := storage.NewFilesystemEngine(tempDir, nil, "")
	if err != nil {
		t.Fatalf("failed to initialize storage engine: %v", err)
	}
	defer engine.Close()

	creds := auth.NewCredentials("access", "secret")
	handler := NewHandler(Options{Engine: engine, Creds: creds, S3Port: 9000, Region: "us-east-1", LoginRateLimit: 100, APIRateLimit: 1000})

	if err := engine.CreateBucket("share-bucket"); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}

	token, err := GenerateToken("access")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/buckets/share-bucket/objects/presign?key=report.txt&expires=3600&download=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse presign response: %v", err)
	}

	parsed, err := url.Parse(resp.URL)
	if err != nil {
		t.Fatalf("failed to parse presigned URL: %v", err)
	}
	disposition := parsed.Query().Get("response-content-disposition")
	if disposition == "" {
		t.Fatal("expected response-content-disposition in the signed query string when download=1")
	}
	if !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, "report.txt") {
		t.Errorf("response-content-disposition = %q, want attachment with filename report.txt", disposition)
	}
}

// TestPresignObjectIgnoresXForwardedProtoWhenNotTrusted guards against a gap
// where X-Forwarded-Proto was trusted unconditionally, unlike the
// TrustProxy-gated X-Forwarded-For handling elsewhere: a direct, unproxied
// client could set this header to force "https://" into a generated
// presigned link even when Stiva is only listening on plain HTTP, producing
// a broken link.
func TestPresignObjectIgnoresXForwardedProtoWhenNotTrusted(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "console-xfp-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	engine, err := storage.NewFilesystemEngine(tempDir, nil, "")
	if err != nil {
		t.Fatalf("failed to initialize storage engine: %v", err)
	}
	defer engine.Close()

	creds := auth.NewCredentials("access", "secret")
	if err := engine.CreateBucket("xfp-bucket"); err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}
	token, err := GenerateToken("access")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	newPresignReq := func() *http.Request {
		req := httptest.NewRequest("GET", "/api/buckets/xfp-bucket/objects/presign?key=report.txt&expires=3600", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-Proto", "https")
		return req
	}

	presignedURL := func(handler *Handler) string {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newPresignReq())
		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse presign response: %v", err)
		}
		return resp.URL
	}

	untrusted := NewHandler(Options{
		Engine: engine, Creds: creds, S3Port: 9000, Region: "us-east-1",
		LoginRateLimit: 100, APIRateLimit: 1000, TrustProxy: false,
	})
	if url := presignedURL(untrusted); !strings.HasPrefix(url, "http://") {
		t.Errorf("with TrustProxy=false, X-Forwarded-Proto must be ignored; got URL %q, want http:// scheme", url)
	}

	trusted := NewHandler(Options{
		Engine: engine, Creds: creds, S3Port: 9000, Region: "us-east-1",
		LoginRateLimit: 100, APIRateLimit: 1000, TrustProxy: true,
	})
	if url := presignedURL(trusted); !strings.HasPrefix(url, "https://") {
		t.Errorf("with TrustProxy=true, X-Forwarded-Proto should be honored; got URL %q, want https:// scheme", url)
	}
}

// TestGetConfigReportsActualS3Port guards against the console's top-bar
// badge hardcoding "Port 9000" regardless of the operator's actual
// STIVA_S3_PORT.
func TestGetConfigReportsActualS3Port(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "console-config-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	engine, err := storage.NewFilesystemEngine(tempDir, nil, "")
	if err != nil {
		t.Fatalf("failed to initialize storage engine: %v", err)
	}
	defer engine.Close()

	creds := auth.NewCredentials("access", "secret")
	handler := NewHandler(Options{
		Engine: engine, Creds: creds, S3Port: 9900, Region: "us-east-1",
		LoginRateLimit: 100, APIRateLimit: 1000,
	})

	token, err := GenerateToken("access")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		S3Port int `json:"s3Port"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse config response: %v", err)
	}
	if resp.S3Port != 9900 {
		t.Errorf("s3Port = %d, want 9900 (the configured port, not a hardcoded default)", resp.S3Port)
	}
}

func TestConsoleRateLimiting(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "console-rl-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	engine, err := storage.NewFilesystemEngine(tempDir, nil, "")
	if err != nil {
		t.Fatalf("failed to initialize storage engine: %v", err)
	}
	defer engine.Close()

	creds := auth.NewCredentials("access", "secret")
	// Set very tight rate limit (burst of 1 for login, burst of 2 for API)
	handler := NewHandler(Options{Engine: engine, Creds: creds, S3Port: 9000, Region: "us-east-1", LoginRateLimit: 1, APIRateLimit: 2})

	// Test login rate limiting
	loginBody, _ := json.Marshal(map[string]string{
		"accessKey": "access",
		"secretKey": "secret",
	})

	// Request 1: should pass
	req := httptest.NewRequest("POST", "/api/login", bytes.NewReader(loginBody))
	req.RemoteAddr = "1.2.3.4:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Request 2 (immediate): should fail with 429
	req = httptest.NewRequest("POST", "/api/login", bytes.NewReader(loginBody))
	req.RemoteAddr = "1.2.3.4:1234"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}

	// Request 3 (from different IP): should pass
	req = httptest.NewRequest("POST", "/api/login", bytes.NewReader(loginBody))
	req.RemoteAddr = "5.6.7.8:1234"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}
