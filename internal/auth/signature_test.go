package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// signRequest is a test helper that signs an HTTP request using AWS SigV4.
func signRequest(r *http.Request, creds *Credentials, region, service string, t time.Time) {
	datestamp := t.Format("20060102")
	amzDate := t.Format("20060102T150405Z")

	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("Host", r.Host)

	// Build canonical headers and signed headers
	signedHeaders := []string{"host", "x-amz-date"}

	var canonicalHeaders strings.Builder
	fmt.Fprintf(&canonicalHeaders, "host:%s\n", r.Host)
	fmt.Fprintf(&canonicalHeaders, "x-amz-date:%s\n", amzDate)

	payloadHash := hashSHA256([]byte(""))
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Canonical request
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		r.Method,
		r.URL.Path,
		r.URL.RawQuery,
		canonicalHeaders.String(),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	)

	// String to sign
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate,
		scope,
		hashSHA256([]byte(canonicalRequest)),
	)

	// Derive signing key
	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	// Sign
	h := hmac.New(sha256.New, kSigning)
	h.Write([]byte(stringToSign))
	signature := hex.EncodeToString(h.Sum(nil))

	// Build authorization header
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey,
		scope,
		strings.Join(signedHeaders, ";"),
		signature,
	)
	r.Header.Set("Authorization", authHeader)
}

func TestVerifyValidSignature(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	req, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req.Host = "localhost:9000"

	now := time.Now().UTC()
	signRequest(req, creds, "us-east-1", "s3", now)

	if err := verifier.Verify(req); err != nil {
		t.Errorf("expected valid signature, got error: %v", err)
	}
}

func TestVerifyInvalidAccessKey(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	wrongCreds := NewCredentials("WRONGACCESSKEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	req, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req.Host = "localhost:9000"

	now := time.Now().UTC()
	signRequest(req, wrongCreds, "us-east-1", "s3", now)

	err := verifier.Verify(req)
	if err == nil {
		t.Error("expected error for invalid access key")
	}
	if err != nil {
		var authErr *AuthError
		ok := errors.As(err, &authErr)
		if !ok {
			t.Errorf("expected *AuthError, got %T", err)
		} else if authErr.Code != "InvalidAccessKeyId" {
			t.Errorf("expected Code 'InvalidAccessKeyId', got %q", authErr.Code)
		}
	}
}

func TestVerifyWrongSecretKey(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	wrongCreds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "WRONGSECRETKEYWRONGSECRETKEYWRONGKEY")
	verifier := NewSigV4Verifier(creds)

	req, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req.Host = "localhost:9000"

	now := time.Now().UTC()
	signRequest(req, wrongCreds, "us-east-1", "s3", now)

	err := verifier.Verify(req)
	if err == nil {
		t.Error("expected error for wrong secret key")
	}
	if err != nil {
		var authErr *AuthError
		ok := errors.As(err, &authErr)
		if !ok {
			t.Errorf("expected *AuthError, got %T", err)
		} else if authErr.Code != "SignatureDoesNotMatch" {
			t.Errorf("expected Code 'SignatureDoesNotMatch', got %q", authErr.Code)
		}
	}
}

func TestVerifyMissingAuthHeader(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	req, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req.Host = "localhost:9000"

	err := verifier.Verify(req)
	if err == nil {
		t.Error("expected error for missing Authorization header")
	}
}

func TestVerifyMissingDateHeader(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	req, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req.Host = "localhost:9000"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc123")

	err := verifier.Verify(req)
	if err == nil {
		t.Error("expected error for missing date header")
	}
}

func TestVerifyPutRequest(t *testing.T) {
	creds := NewCredentials("myaccesskey", "mysecretkey")
	verifier := NewSigV4Verifier(creds)

	req, _ := http.NewRequest("PUT", "http://localhost:9000/test-bucket/test-key.txt", nil)
	req.Host = "localhost:9000"

	now := time.Now().UTC()
	signRequest(req, creds, "us-east-1", "s3", now)

	if err := verifier.Verify(req); err != nil {
		t.Errorf("expected valid signature for PUT, got error: %v", err)
	}
}

func TestCredentialsIsValid(t *testing.T) {
	tests := []struct {
		name      string
		accessKey string
		secretKey string
		valid     bool
	}{
		{"both set", "access", "secret", true},
		{"empty access", "", "secret", false},
		{"empty secret", "access", "", false},
		{"both empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds := NewCredentials(tt.accessKey, tt.secretKey)
			if creds.IsValid() != tt.valid {
				t.Errorf("expected IsValid=%v for (%q, %q)", tt.valid, tt.accessKey, tt.secretKey)
			}
		})
	}
}

func TestDeriveSigningKeyConsistency(t *testing.T) {
	creds := NewCredentials("access", "secret")
	verifier := NewSigV4Verifier(creds)

	key1 := verifier.deriveSigningKey("20260101", "us-east-1", "s3")
	key2 := verifier.deriveSigningKey("20260101", "us-east-1", "s3")

	if !hmac.Equal(key1, key2) {
		t.Error("same inputs should produce same signing key")
	}

	// Different date should produce different key
	key3 := verifier.deriveSigningKey("20260102", "us-east-1", "s3")
	if hmac.Equal(key1, key3) {
		t.Error("different dates should produce different signing keys")
	}
}

func signRequestWithBody(r *http.Request, creds *Credentials, region, service string, t time.Time, body []byte, customHash string) {
	datestamp := t.Format("20060102")
	amzDate := t.Format("20060102T150405Z")

	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("Host", r.Host)

	// Build canonical headers and signed headers
	signedHeaders := []string{"host", "x-amz-date"}

	var canonicalHeaders strings.Builder
	fmt.Fprintf(&canonicalHeaders, "host:%s\n", r.Host)
	fmt.Fprintf(&canonicalHeaders, "x-amz-date:%s\n", amzDate)

	payloadHash := customHash
	if payloadHash == "" {
		payloadHash = hashSHA256(body)
	}
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Set request body if any
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	// Canonical request
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		r.Method,
		r.URL.Path,
		r.URL.RawQuery,
		canonicalHeaders.String(),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	)

	// String to sign
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate,
		scope,
		hashSHA256([]byte(canonicalRequest)),
	)

	// Derive signing key
	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	// Sign
	h := hmac.New(sha256.New, kSigning)
	h.Write([]byte(stringToSign))
	signature := hex.EncodeToString(h.Sum(nil))

	// Build authorization header
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey,
		scope,
		strings.Join(signedHeaders, ";"),
		signature,
	)
	r.Header.Set("Authorization", authHeader)
}

func TestVerifySignatureTimeDrift(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	// Request from 20 minutes in the past
	req1, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req1.Host = "localhost:9000"
	signRequest(req1, creds, "us-east-1", "s3", time.Now().UTC().Add(-20*time.Minute))

	if err := verifier.Verify(req1); err == nil {
		t.Error("expected signature validation to fail for 20-minute old request")
	}

	// Request from 20 minutes in the future
	req2, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req2.Host = "localhost:9000"
	signRequest(req2, creds, "us-east-1", "s3", time.Now().UTC().Add(20*time.Minute))

	if err := verifier.Verify(req2); err == nil {
		t.Error("expected signature validation to fail for request 20 minutes in the future")
	}

	// Request within 5 minutes should pass
	req3, _ := http.NewRequest("GET", "http://localhost:9000/", nil)
	req3.Host = "localhost:9000"
	signRequest(req3, creds, "us-east-1", "s3", time.Now().UTC().Add(-5*time.Minute))

	if err := verifier.Verify(req3); err != nil {
		t.Errorf("expected signature to pass for 5-minute old request, got: %v", err)
	}
}

func TestVerifySignaturePayloadMismatch(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	body := []byte("expected body content")

	// 1. Signature with matching body - should pass
	req1, _ := http.NewRequest("POST", "http://localhost:9000/", nil)
	req1.Host = "localhost:9000"
	signRequestWithBody(req1, creds, "us-east-1", "s3", time.Now().UTC(), body, "")

	if err := verifier.Verify(req1); err != nil {
		t.Errorf("expected matching payload signature to pass, got: %v", err)
	}

	// 2. Signature with mismatched body - should fail
	req2, _ := http.NewRequest("POST", "http://localhost:9000/", nil)
	req2.Host = "localhost:9000"
	signRequestWithBody(req2, creds, "us-east-1", "s3", time.Now().UTC(), body, "")
	// Tamper with body AFTER signature was generated
	req2.Body = io.NopCloser(bytes.NewReader([]byte("tampered body content")))

	if err := verifier.Verify(req2); err == nil {
		t.Error("expected signature to fail when payload body is tampered")
	}
}

func TestCanonicalQueryStringEncoding(t *testing.T) {
	values := make(url.Values)
	values.Set("key with space", "value with space")
	values.Set("key/with/slash", "value/with/slash")

	canonical := getCanonicalQueryString(values)

	expected := "key%20with%20space=value%20with%20space&key%2Fwith%2Fslash=value%2Fwith%2Fslash"
	if canonical != expected {
		t.Errorf("expected canonical query string:\n%q\ngot:\n%q", expected, canonical)
	}
}
