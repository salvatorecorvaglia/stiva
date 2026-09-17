package s3api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hashSHA256(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func signTestRequest(r *http.Request, accessKey, secretKey string, region, service string, t time.Time, body []byte) {
	if time.Now().UTC().Sub(t).Abs() > 15*time.Minute {
		// Manual signing for clock skew validation
		datestamp := t.Format("20060102")
		amzDate := t.Format("20060102T150405Z")
		r.Header.Set("X-Amz-Date", amzDate)
		r.Header.Set("Host", r.Host)
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

		var canonicalHeaders strings.Builder
		fmt.Fprintf(&canonicalHeaders, "host:%s\n", r.Host)
		fmt.Fprintf(&canonicalHeaders, "x-amz-date:%s\n", amzDate)

		canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\nhost;x-amz-date\nUNSIGNED-PAYLOAD",
			r.Method, r.URL.Path, r.URL.RawQuery, canonicalHeaders.String())

		scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)
		stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
			amzDate, scope, hashSHA256([]byte(canonicalRequest)))

		kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(datestamp))
		kRegion := hmacSHA256(kDate, []byte(region))
		kService := hmacSHA256(kRegion, []byte(service))
		kSigning := hmacSHA256(kService, []byte("aws4_request"))

		h := hmac.New(sha256.New, kSigning)
		h.Write([]byte(stringToSign))
		signature := hex.EncodeToString(h.Sum(nil))

		authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=host;x-amz-date, Signature=%s",
			accessKey, scope, signature)
		r.Header.Set("Authorization", authHeader)
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		return
	}

	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	auth.SignRequest(r, accessKey, secretKey, region, service)
}

func TestResolveBucketAndKey(t *testing.T) {
	tests := []struct {
		name           string
		domain         string
		host           string
		path           string
		expectedBucket string
		expectedKey    string
	}{
		{
			name:           "Path style (no domain config)",
			domain:         "",
			host:           "localhost:9000",
			path:           "/mybucket/mykey/file.txt",
			expectedBucket: "mybucket",
			expectedKey:    "mykey/file.txt",
		},
		{
			name:           "Path style (with domain config)",
			domain:         "stiva.local",
			host:           "stiva.local:9000",
			path:           "/mybucket/mykey/file.txt",
			expectedBucket: "mybucket",
			expectedKey:    "mykey/file.txt",
		},
		{
			name:           "Virtual host style",
			domain:         "stiva.local",
			host:           "mybucket.stiva.local:9000",
			path:           "/mykey/file.txt",
			expectedBucket: "mybucket",
			expectedKey:    "mykey/file.txt",
		},
		{
			name:           "Virtual host style root path",
			domain:         "stiva.local",
			host:           "mybucket.stiva.local:9000",
			path:           "/",
			expectedBucket: "mybucket",
			expectedKey:    "",
		},
		{
			name:           "Root path style (ListBuckets)",
			domain:         "stiva.local",
			host:           "stiva.local:9000",
			path:           "/",
			expectedBucket: "",
			expectedKey:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewRouter(RouterOptions{
				Domain: tc.domain,
			})
			req := httptest.NewRequest("GET", tc.path, nil)
			req.Host = tc.host

			bucket, key := rt.resolveBucketAndKey(req)
			if bucket != tc.expectedBucket {
				t.Errorf("expected bucket %q, got %q", tc.expectedBucket, bucket)
			}
			if key != tc.expectedKey {
				t.Errorf("expected key %q, got %q", tc.expectedKey, key)
			}
		})
	}
}

func setupTestRouter(t *testing.T) (*Router, storage.Engine, *auth.Credentials) {
	tempDir := t.TempDir()
	engine, err := storage.NewFilesystemEngine(tempDir, nil, "")
	if err != nil {
		t.Fatalf("failed to create storage engine: %v", err)
	}

	creds := auth.NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	router := NewRouter(RouterOptions{
		Engine: engine,
		Creds:  creds,
		Region: "us-east-1",
	})

	return router, engine, creds
}

func TestS3APIEndToEnd(t *testing.T) {
	router, engine, creds := setupTestRouter(t)
	defer engine.Close()
	defer router.Close()

	// 1. List Buckets (Empty)
	t.Run("ListBuckets Empty", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://localhost:9000/", nil)
		req.Host = "localhost:9000"
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "<ListAllMyBucketsResult") {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})

	// 2. Create Bucket
	t.Run("CreateBucket", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "http://localhost:9000/test-bucket", nil)
		req.Host = "localhost:9000"
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	// 3. Put Object
	bodyContent := []byte("Hello, Stiva S3!")
	t.Run("PutObject", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "http://localhost:9000/test-bucket/hello.txt", bytes.NewReader(bodyContent))
		req.Host = "localhost:9000"
		req.Header.Set("Content-Type", "text/plain")
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), bodyContent)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("expected ETag header in response")
		}
	})

	// 4. Get Object
	t.Run("GetObject", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://localhost:9000/test-bucket/hello.txt", nil)
		req.Host = "localhost:9000"
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != string(bodyContent) {
			t.Errorf("expected body %q, got %q", string(bodyContent), rec.Body.String())
		}
	})

	// 5. Head Object
	t.Run("HeadObject", func(t *testing.T) {
		req := httptest.NewRequest("HEAD", "http://localhost:9000/test-bucket/hello.txt", nil)
		req.Host = "localhost:9000"
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		if rec.Header().Get("Content-Length") != fmt.Sprintf("%d", len(bodyContent)) {
			t.Errorf("expected Content-Length %d, got %s", len(bodyContent), rec.Header().Get("Content-Length"))
		}
	})

	// 6. List Objects V2
	t.Run("ListObjectsV2", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://localhost:9000/test-bucket?list-type=2", nil)
		req.Host = "localhost:9000"
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "<Key>hello.txt</Key>") {
			t.Errorf("expected key hello.txt in list response: %s", rec.Body.String())
		}
	})

	// 7. Delete Object
	t.Run("DeleteObject", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "http://localhost:9000/test-bucket/hello.txt", nil)
		req.Host = "localhost:9000"
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected status 204, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	// 8. Delete Bucket
	t.Run("DeleteBucket", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "http://localhost:9000/test-bucket", nil)
		req.Host = "localhost:9000"
		signTestRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected status 204, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestUnauthenticatedRequestRejected(t *testing.T) {
	router, engine, _ := setupTestRouter(t)
	defer engine.Close()
	defer router.Close()

	req := httptest.NewRequest("GET", "http://localhost:9000/", nil)
	req.Host = "localhost:9000"
	// No Authorization header

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 AccessDenied, got %d", rec.Code)
	}
}

func TestInvalidSignatureRejected(t *testing.T) {
	router, engine, _ := setupTestRouter(t)
	defer engine.Close()
	defer router.Close()

	req := httptest.NewRequest("GET", "http://localhost:9000/", nil)
	req.Host = "localhost:9000"
	signTestRequest(req, "AKIAIOSFODNN7EXAMPLE", "WRONGSECRETKEY", "us-east-1", "s3", time.Now().UTC(), nil)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 SignatureDoesNotMatch, got %d", rec.Code)
	}
}

func TestPresignedURLRouting(t *testing.T) {
	router, engine, creds := setupTestRouter(t)
	defer engine.Close()
	defer router.Close()

	// 1. Create a bucket first
	reqCreate := httptest.NewRequest("PUT", "http://localhost:9000/presign-bucket", nil)
	reqCreate.Host = "localhost:9000"
	signTestRequest(reqCreate, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC(), nil)
	recCreate := httptest.NewRecorder()
	router.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusOK {
		t.Fatalf("failed to create bucket: %s", recCreate.Body.String())
	}

	// 2. Put object using presigned URL
	reqPut := httptest.NewRequest("PUT", "http://localhost:9000/presign-bucket/test.txt", bytes.NewReader([]byte("presigned data")))
	reqPut.Host = "localhost:9000"

	datestamp := time.Now().UTC().Format("20060102")
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	scope := datestamp + "/us-east-1/s3/aws4_request"

	q := reqPut.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", creds.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", "3600")
	q.Set("X-Amz-SignedHeaders", "host")

	reqPut.URL.RawQuery = q.Encode()

	canonical := buildPresignedCanonicalRequest(reqPut, []string{"host"})
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + testSHA256Hex([]byte(canonical))

	kDate := testHMACSHA256([]byte("AWS4"+creds.SecretKey), []byte(datestamp))
	kRegion := testHMACSHA256(kDate, []byte("us-east-1"))
	kService := testHMACSHA256(kRegion, []byte("s3"))
	kSigning := testHMACSHA256(kService, []byte("aws4_request"))
	sig := testHMACSHA256(kSigning, []byte(stringToSign))

	q.Set("X-Amz-Signature", hex.EncodeToString(sig))
	reqPut.URL.RawQuery = q.Encode()

	recPut := httptest.NewRecorder()
	router.ServeHTTP(recPut, reqPut)

	if recPut.Code != http.StatusOK {
		t.Fatalf("presigned PUT failed: status=%d, body=%s", recPut.Code, recPut.Body.String())
	}

	// 3. Verify object content was written
	objData, _, err := engine.GetObject(context.Background(), "presign-bucket", "test.txt", "")
	if err != nil {
		t.Fatalf("failed to read object: %v", err)
	}
	defer objData.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(objData)
	if buf.String() != "presigned data" {
		t.Errorf("expected 'presigned data', got '%s'", buf.String())
	}
}

func TestClockSkewRejection(t *testing.T) {
	router, engine, creds := setupTestRouter(t)
	defer engine.Close()
	defer router.Close()

	// 1. Request from 20 minutes in the past
	req1 := httptest.NewRequest("GET", "http://localhost:9000/", nil)
	req1.Host = "localhost:9000"
	signTestRequest(req1, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC().Add(-20*time.Minute), nil)

	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusForbidden {
		t.Errorf("expected status 403 RequestTimeTooSkewed for old request, got %d", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), "RequestTimeTooSkewed") {
		t.Errorf("expected RequestTimeTooSkewed code, got: %s", rec1.Body.String())
	}

	// 2. Request from 20 minutes in the future
	req2 := httptest.NewRequest("GET", "http://localhost:9000/", nil)
	req2.Host = "localhost:9000"
	signTestRequest(req2, creds.AccessKey, creds.SecretKey, "us-east-1", "s3", time.Now().UTC().Add(20*time.Minute), nil)

	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected status 403 RequestTimeTooSkewed for future request, got %d", rec2.Code)
	}
}

// buildPresignedCanonicalRequest mirrors the canonical request the verifier
// builds for a presigned URL: the signature parameter is excluded, and the
// payload hash is always UNSIGNED-PAYLOAD.
func buildPresignedCanonicalRequest(r *http.Request, signedHeaders []string) string {
	q := r.URL.Query()
	q.Del("X-Amz-Signature")

	var headers strings.Builder
	sorted := append([]string(nil), signedHeaders...)
	sort.Strings(sorted)
	for _, h := range sorted {
		h = strings.ToLower(strings.TrimSpace(h))
		v := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && v == "" {
			v = r.Host
		}
		headers.WriteString(h + ":" + v + "\n")
	}

	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		r.Method, path, testCanonicalQuery(q), headers.String(),
		strings.Join(sorted, ";"), "UNSIGNED-PAYLOAD")
}
