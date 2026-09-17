package auth

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
)

// TestHashPayloadCapsChunkedBodyRegardlessOfContentLength guards against a
// gap where MaxPayloadSize was only checked against r.ContentLength.
// A request sent with Transfer-Encoding: chunked reports ContentLength == -1,
// so that check never fired, and the body was then spooled to a temp file
// with no size cap at all — an authenticated client could exhaust disk space
// with a single oversized chunked request.
func TestHashPayloadCapsChunkedBodyRegardlessOfContentLength(t *testing.T) {
	orig := MaxPayloadSize
	MaxPayloadSize = 3 << 20 // 3MB limit for this test
	t.Cleanup(func() { MaxPayloadSize = orig })

	// Larger than both the in-memory threshold and the configured limit, so
	// this exercises the temp-file spooling path where the bug lived.
	body := bytes.Repeat([]byte("a"), 10<<20) // 10MB
	req, err := http.NewRequest(http.MethodPut, "http://localhost/x", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.ContentLength = -1 // simulate Transfer-Encoding: chunked

	_, err = HashPayload(req)
	if err == nil {
		t.Fatal("expected an EntityTooLarge error for an oversized chunked body, got nil")
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
	if authErr.Code != "EntityTooLarge" {
		t.Errorf("error code = %q, want EntityTooLarge", authErr.Code)
	}
}
