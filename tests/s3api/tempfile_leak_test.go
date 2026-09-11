package s3api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/s3api"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// signPut signs a PUT with a real (non-UNSIGNED) payload hash, which is what
// drives auth.HashPayload down its spool-to-disk path for bodies over 2MiB.
func signPut(r *http.Request, creds *auth.Credentials, region string, now time.Time, body []byte) {
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	payloadHash := auth.HashSHA256(body)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", r.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	// The canonical query string is not the raw one: AWS percent-encodes each
	// key and value (so a value containing "/" becomes "%2F") and sorts them.
	// Use the same helper the verifier does.
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		r.Method, r.URL.EscapedPath(), auth.GetCanonicalQueryString(r.URL.Query()),
		canonicalHeaders, signedHeaders, payloadHash)

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", datestamp, region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, auth.HashSHA256([]byte(canonicalRequest)))

	kDate := auth.HmacSHA256([]byte("AWS4"+creds.SecretKey), []byte(datestamp))
	kRegion := auth.HmacSHA256(kDate, []byte(region))
	kService := auth.HmacSHA256(kRegion, []byte("s3"))
	kSigning := auth.HmacSHA256(kService, []byte("aws4_request"))

	mac := hmac.New(sha256.New, kSigning)
	mac.Write([]byte(stringToSign))

	r.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope, signedHeaders, hex.EncodeToString(mac.Sum(nil))))
}

func spooledBodies(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "stiva-body-") || strings.HasPrefix(e.Name(), "stiva-chunked-") {
			left = append(left, e.Name())
		}
	}
	return left
}

// TestSignedPutDoesNotLeakSpooledBody covers a disk leak in the SigV4 layer.
//
// auth.HashPayload spools a request body larger than 2MiB to a temp file and
// swaps r.Body for a reader that deletes that file on Close. Nothing ever
// called Close: net/http closes the *original* body it captured when reading
// the request, not the replacement, and no handler closed r.Body either. Every
// signed request over 2MiB therefore left a stiva-body-* file behind forever.
func TestSignedPutDoesNotLeakSpooledBody(t *testing.T) {
	dataDir := t.TempDir()
	tempDir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}

	origTempDir := auth.TempDir
	auth.TempDir = tempDir
	t.Cleanup(func() { auth.TempDir = origTempDir })

	eng, err := storage.NewFilesystemEngine(dataDir, nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	creds := auth.NewCredentials("ak", "sk")
	rt := s3api.NewRouter(s3api.RouterOptions{Engine: eng, Creds: creds, Region: "us-east-1"})
	t.Cleanup(rt.Close)

	if err := eng.CreateBucket("leak"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// Over the 2MiB in-memory threshold, so the body is spooled to disk.
	body := bytes.Repeat([]byte("a"), 3<<20)

	req := httptest.NewRequest(http.MethodPut, "/leak/big.bin", nil)
	req.Host = "example.com"
	signPut(req, creds, "us-east-1", time.Now().UTC(), body)

	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if leaked := spooledBodies(t, tempDir); len(leaked) > 0 {
		t.Errorf("spooled request body was not cleaned up: %v", leaked)
	}
}

// TestRejectedSignatureDoesNotLeakSpooledBody covers the same leak on the
// failure path: the body is hashed (and spooled) before the signature is
// compared, so a request that is ultimately rejected must still clean up.
func TestRejectedSignatureDoesNotLeakSpooledBody(t *testing.T) {
	dataDir := t.TempDir()
	tempDir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}

	origTempDir := auth.TempDir
	auth.TempDir = tempDir
	t.Cleanup(func() { auth.TempDir = origTempDir })

	eng, err := storage.NewFilesystemEngine(dataDir, nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	creds := auth.NewCredentials("ak", "sk")
	rt := s3api.NewRouter(s3api.RouterOptions{Engine: eng, Creds: creds, Region: "us-east-1"})
	t.Cleanup(rt.Close)

	if err := eng.CreateBucket("leak"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	body := bytes.Repeat([]byte("b"), 3<<20)
	req := httptest.NewRequest(http.MethodPut, "/leak/big.bin", nil)
	req.Host = "example.com"
	signPut(req, creds, "us-east-1", time.Now().UTC(), body)
	// Corrupt the signature so verification fails after the body was spooled.
	req.Header.Set("Authorization", strings.Replace(
		req.Header.Get("Authorization"), "Signature=", "Signature=00", 1))

	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if leaked := spooledBodies(t, tempDir); len(leaked) > 0 {
		t.Errorf("spooled request body was not cleaned up after a rejected signature: %v", leaked)
	}
}
