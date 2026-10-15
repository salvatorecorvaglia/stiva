package auth

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
)

// TestSignedPayloadCapsChunkedBodyRegardlessOfContentLength guards against a
// gap where the payload limit was only checked against r.ContentLength. A request
// sent with Transfer-Encoding: chunked reports ContentLength == -1, so that
// check never fires; the body must still be cut off at the limit as it is read.
func TestSignedPayloadCapsChunkedBodyRegardlessOfContentLength(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 10<<20) // 10MB, past both the in-memory threshold and the limit
	req, err := http.NewRequest(http.MethodPut, "http://localhost/x", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.ContentLength = -1 // simulate Transfer-Encoding: chunked

	const limit = 3 << 20 // 3MB limit for this test
	if err := verifySignedPayload(req, testSHA256Hex(body), limit); err != nil {
		t.Fatalf("verifySignedPayload: %v", err)
	}
	_, err = io.ReadAll(req.Body)

	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != "EntityTooLarge" {
		t.Fatalf("reading an oversized chunked body: err = %v, want EntityTooLarge", err)
	}
}
