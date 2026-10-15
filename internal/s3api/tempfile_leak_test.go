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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// Local signing primitives. These used to call exported wrappers in the auth
// package that existed only for these tests; a few lines of crypto/hmac here
// keeps auth's API surface to what production actually uses.

func testHMACSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func testSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// testCanonicalQuery builds the SigV4 canonical query string: every key and
// value percent-encoded (so "/" becomes "%2F") and sorted, which is not the
// same as the raw query.
func testCanonicalQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	enc := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			c := s[i]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
				c == '_' || c == '-' || c == '~' || c == '.' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
		return b.String()
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, enc(k)+"="+enc(v))
		}
	}
	return strings.Join(pairs, "&")
}

// signPut signs a PUT with a real (non-UNSIGNED) payload hash, which is what
// makes the SigV4 layer verify the body against it.
func signPut(r *http.Request, creds *auth.Credentials, region string, now time.Time, body []byte) {
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	payloadHash := testSHA256Hex(body)
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
		r.Method, r.URL.EscapedPath(), testCanonicalQuery(r.URL.Query()),
		canonicalHeaders, signedHeaders, payloadHash)

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", datestamp, region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, testSHA256Hex([]byte(canonicalRequest)))

	kDate := testHMACSHA256([]byte("AWS4"+creds.SecretKey), []byte(datestamp))
	kRegion := testHMACSHA256(kDate, []byte(region))
	kService := testHMACSHA256(kRegion, []byte("s3"))
	kSigning := testHMACSHA256(kService, []byte("aws4_request"))

	mac := hmac.New(sha256.New, kSigning)
	mac.Write([]byte(stringToSign))

	r.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope, signedHeaders, hex.EncodeToString(mac.Sum(nil))))
}

// spooledBodies lists request-body temp files anywhere under dir. Payloads
// are now verified as they stream into the object, so none should ever exist.
func spooledBodies(t *testing.T, dir string) []string {
	t.Helper()
	var left []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), "stiva-body-") || strings.HasPrefix(d.Name(), "stiva-chunked-") {
			left = append(left, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return left
}

// TestSignedPutDoesNotLeakSpooledBody covers a disk leak in the SigV4 layer.
//
// Signed bodies over 2MiB used to be spooled to a temp file to be hashed, and
// for a long time nothing deleted it: every such request left a stiva-body-*
// file behind. Bodies are now hashed as they stream into the object, so a
// large signed PUT must leave no request-body file anywhere.
func TestSignedPutDoesNotLeakSpooledBody(t *testing.T) {
	dataDir := t.TempDir()

	eng, err := storage.NewFilesystemEngine(dataDir, nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	creds := auth.NewCredentials("ak", "sk")
	rt := NewRouter(RouterOptions{Engine: eng, Creds: creds, Region: "us-east-1"})
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
	if leaked := spooledBodies(t, dataDir); len(leaked) > 0 {
		t.Errorf("spooled request body was not cleaned up: %v", leaked)
	}
}

// TestRejectedSignatureDoesNotLeakSpooledBody covers the same leak on the
// failure path: a rejected request must leave nothing behind either.
func TestRejectedSignatureDoesNotLeakSpooledBody(t *testing.T) {
	dataDir := t.TempDir()

	eng, err := storage.NewFilesystemEngine(dataDir, nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	creds := auth.NewCredentials("ak", "sk")
	rt := NewRouter(RouterOptions{Engine: eng, Creds: creds, Region: "us-east-1"})
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
	if leaked := spooledBodies(t, dataDir); len(leaked) > 0 {
		t.Errorf("spooled request body was not cleaned up after a rejected signature: %v", leaked)
	}
}

// TestTamperedLargeSignedPutIsNotStored: a body over 2MiB is now checked
// against its signed SHA-256 while it streams into the object, and a mismatch
// must still abort the upload — with S3's error code — and store nothing.
func TestTamperedLargeSignedPutIsNotStored(t *testing.T) {
	dataDir := t.TempDir()
	eng, err := storage.NewFilesystemEngine(dataDir, nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	creds := auth.NewCredentials("ak", "sk")
	rt := NewRouter(RouterOptions{Engine: eng, Creds: creds, Region: "us-east-1"})
	t.Cleanup(rt.Close)
	if err := eng.CreateBucket("tamper"); err != nil {
		t.Fatal(err)
	}

	body := bytes.Repeat([]byte("t"), 3<<20)
	req := httptest.NewRequest(http.MethodPut, "/tamper/big.bin", nil)
	req.Host = "example.com"
	signPut(req, creds, "us-east-1", time.Now().UTC(), body)
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] = 'X'
	req.Body = io.NopCloser(bytes.NewReader(tampered))

	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "XAmzContentSHA256Mismatch") {
		t.Fatalf("status %d body %s; want 400 XAmzContentSHA256Mismatch", w.Code, w.Body.String())
	}
	if _, err := eng.HeadObject(context.Background(), "tamper", "big.bin", ""); err == nil {
		t.Error("a body that failed its hash check was stored")
	}
}

// TestLargeSignedBodyToConfigEndpointIsRejected: only object and part uploads
// accept a body that is verified while it is read, since they read to the end
// before committing. Everything else takes at most maxXMLRequestBody, read and
// checked in full first — a larger body is refused outright.
func TestLargeSignedBodyToConfigEndpointIsRejected(t *testing.T) {
	eng, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	creds := auth.NewCredentials("ak", "sk")
	rt := NewRouter(RouterOptions{Engine: eng, Creds: creds, Region: "us-east-1"})
	t.Cleanup(rt.Close)
	if err := eng.CreateBucket("cfg"); err != nil {
		t.Fatal(err)
	}

	body := append([]byte("<CORSConfiguration>"), bytes.Repeat([]byte(" "), 3<<20)...)
	req := httptest.NewRequest(http.MethodPut, "/cfg?cors", nil)
	req.Host = "example.com"
	signPut(req, creds, "us-east-1", time.Now().UTC(), body)

	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "MaxMessageLengthExceeded") {
		t.Fatalf("status %d body %s; want 400 MaxMessageLengthExceeded", w.Code, w.Body.String())
	}
}
