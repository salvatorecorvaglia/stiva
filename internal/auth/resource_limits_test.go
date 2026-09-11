package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func testCreds() *Credentials {
	return NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
}

func authErrCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
	return authErr.Code
}

// TestVerifyRejectsForeignCredentialScope covers the unbounded growth of the
// derived-signing-key cache. Its key is datestamp/region/service, taken
// verbatim from the caller's credential scope and never evicted, and it was
// populated before the signature was compared — so anyone holding the
// (non-secret) access key could add a permanent entry per request just by
// varying the region or service. Rejecting scopes no real S3 client would send
// keeps that key space to a handful of values.
func TestVerifyRejectsForeignCredentialScope(t *testing.T) {
	creds := testCreds()
	verifier := NewSigV4Verifier(creds)

	tests := []struct {
		name    string
		region  string
		service string
	}{
		{"non-s3 service", "us-east-1", "iam"},
		{"attacker-chosen service", "us-east-1", "svc-0001"},
		{"oversized region", strings.Repeat("r", 200), "s3"},
		{"empty region", "", "s3"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "http://localhost:9000/", nil)
			req.Host = "localhost:9000"
			signRequest(req, creds, tc.region, tc.service, time.Now().UTC())

			if code := authErrCode(t, verifier.Verify(req)); code != "AuthorizationHeaderMalformed" {
				t.Errorf("error code = %q, want AuthorizationHeaderMalformed", code)
			}
		})
	}
}

// TestVerifyAcceptsOrdinaryScope guards the check above against being so strict
// that a normal client breaks: any region is still accepted, as before.
func TestVerifyAcceptsOrdinaryScope(t *testing.T) {
	creds := testCreds()
	verifier := NewSigV4Verifier(creds)

	for _, region := range []string{"us-east-1", "eu-west-3", "custom-region"} {
		req, _ := http.NewRequest(http.MethodGet, "http://localhost:9000/", nil)
		req.Host = "localhost:9000"
		signRequest(req, creds, region, "s3", time.Now().UTC())

		if err := verifier.Verify(req); err != nil {
			t.Errorf("region %q should verify, got: %v", region, err)
		}
	}
}

// TestStreamingRejectsOversizedChunk covers a single-request OOM. A chunk's
// declared size was allocated up front to read it and checked only against
// MaxPayloadSize (5GiB by default), so one chunk header could drive a 5GiB
// allocation before any data arrived. This builds a frame declaring 1GiB while
// sending almost no data at all.
func TestStreamingRejectsOversizedChunk(t *testing.T) {
	creds := testCreds()
	verifier := NewSigV4Verifier(creds)

	// A well-formed streaming request, whose body we then replace with a frame
	// that lies about its size.
	req := buildStreamingRequest(t, creds, [][]byte{[]byte("x")}, -1)

	var body bytes.Buffer
	fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", 1<<30, strings.Repeat("0", 64))
	body.WriteString("tiny\r\n")
	req.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	req.ContentLength = int64(body.Len())

	code := authErrCode(t, verifier.Verify(req))
	if code != "InvalidRequest" && code != "EntityTooLarge" {
		t.Errorf("error code = %q, want InvalidRequest (oversized chunk rejected before allocation)", code)
	}
}

// TestBadSignatureDoesNotSpoolBody covers the ordering of payload hashing
// against signature verification. The body was hashed — spooling anything over
// 2MiB to a temp file — before the signature was compared, so a caller who knew
// only the access key could make the server write up to MaxPayloadSize to disk
// per request and only then be told the signature was wrong.
func TestBadSignatureDoesNotSpoolBody(t *testing.T) {
	tempDir := t.TempDir()
	orig := TempDir
	TempDir = tempDir
	t.Cleanup(func() { TempDir = orig })

	creds := testCreds()
	verifier := NewSigV4Verifier(creds)

	body := bytes.Repeat([]byte("a"), 3<<20) // over the 2MiB in-memory threshold
	req, _ := http.NewRequest(http.MethodPut, "http://localhost:9000/b/k", nil)
	req.Host = "localhost:9000"
	signRequestWithBody(req, creds, "us-east-1", "s3", time.Now().UTC(), body, "")

	// Corrupt the signature: verification must fail before the body is touched.
	req.Header.Set("Authorization", strings.Replace(
		req.Header.Get("Authorization"), "Signature=", "Signature=00", 1))

	if code := authErrCode(t, verifier.Verify(req)); code != "SignatureDoesNotMatch" {
		t.Fatalf("error code = %q, want SignatureDoesNotMatch", code)
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "stiva-body-") {
			t.Errorf("body was spooled to disk before the signature was verified: %s", e.Name())
		}
	}
}
