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
	"strings"
	"testing"
	"time"
)

// buildStreamingRequest signs a PUT request whose body is encoded per the
// aws-chunked / STREAMING-AWS4-HMAC-SHA256-PAYLOAD wire format: a sequence of
// "<hex-size>;chunk-signature=<sig>\r\n<data>\r\n" frames, chained from the
// request's own Authorization signature, terminated by a zero-size chunk.
// tamperChunk, if >= 0, corrupts that chunk's declared signature so callers
// can exercise the rejection path.
func buildStreamingRequest(t *testing.T, creds *Credentials, chunks [][]byte, tamperChunk int) *http.Request {
	t.Helper()

	region, service := "us-east-1", "s3"
	now := time.Now().UTC()
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)

	host := "localhost:9000"
	path := "/testbucket/testkey"

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:STREAMING-AWS4-HMAC-SHA256-PAYLOAD\nx-amz-date:%s\n", host, amzDate)

	canonicalRequest := fmt.Sprintf("PUT\n%s\n%s\n%s\n%s\nSTREAMING-AWS4-HMAC-SHA256-PAYLOAD",
		path, "", canonicalHeaders, strings.Join(signedHeaders, ";"))

	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, hashSHA256([]byte(canonicalRequest)))

	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	signingKey := hmacSHA256(kService, []byte("aws4_request"))

	seedSig := hex.EncodeToString(hmacSHA256Test(signingKey, []byte(stringToSign)))

	emptyHash := hashSHA256(nil)

	var body bytes.Buffer
	prevSig := seedSig
	signChunk := func(data []byte) string {
		sts := fmt.Sprintf("AWS4-HMAC-SHA256-PAYLOAD\n%s\n%s\n%s\n%s\n%s",
			amzDate, scope, prevSig, emptyHash, hashSHA256(data))
		return hex.EncodeToString(hmacSHA256Test(signingKey, []byte(sts)))
	}

	for i, c := range chunks {
		sig := signChunk(c)
		if i == tamperChunk {
			sig = strings.Repeat("0", len(sig))
		}
		prevSig = sig
		fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(c), sig)
		body.Write(c)
		body.WriteString("\r\n")
	}
	finalSig := signChunk(nil)
	if tamperChunk == len(chunks) {
		finalSig = strings.Repeat("0", len(finalSig))
	}
	fmt.Fprintf(&body, "0;chunk-signature=%s\r\n\r\n", finalSig)

	req, err := http.NewRequest(http.MethodPut, "http://"+host+path, io.NopCloser(bytes.NewReader(body.Bytes())))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Host = host
	req.ContentLength = int64(body.Len())
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope, strings.Join(signedHeaders, ";"), seedSig,
	))
	return req
}

func hmacSHA256Test(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func TestVerifyStreamingPayloadDecodesAndValidates(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	chunks := [][]byte{[]byte("hello, "), []byte("world!")}
	req := buildStreamingRequest(t, creds, chunks, -1)

	if err := verifier.Verify(req); err != nil {
		t.Fatalf("expected valid streaming payload to verify, got: %v", err)
	}

	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read decoded body: %v", err)
	}
	want := "hello, world!"
	if string(got) != want {
		t.Errorf("decoded body = %q, want %q (chunk framing/signatures were not stripped correctly)", got, want)
	}
	if req.ContentLength != int64(len(want)) {
		t.Errorf("ContentLength = %d, want %d", req.ContentLength, len(want))
	}
}

func TestVerifyStreamingPayloadRejectsTamperedChunk(t *testing.T) {
	creds := NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	verifier := NewSigV4Verifier(creds)

	chunks := [][]byte{[]byte("hello, "), []byte("world!")}
	req := buildStreamingRequest(t, creds, chunks, 1) // tamper the second chunk's signature

	err := verifier.Verify(req)
	if err == nil {
		t.Fatal("expected an error for a tampered chunk signature, got nil")
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
	if authErr.Code != "SignatureDoesNotMatch" {
		t.Errorf("expected Code 'SignatureDoesNotMatch', got %q", authErr.Code)
	}
}
