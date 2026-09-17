package s3api

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSSECValidationErrorsUseSpecificCodes guards against every SSE-C
// validation failure being collapsed into the generic InvalidArgument error
// code — real S3 returns distinct codes (MissingEncryptionKey,
// InvalidEncryptionKeyMD5, etc.) that some client tooling branches on.
func TestSSECValidationErrorsUseSpecificCodes(t *testing.T) {
	rt, eng := newOpsRouter(t)
	if err := eng.CreateBucket("ssec"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// AES256 declared but no customer key supplied.
	req := httptest.NewRequest(http.MethodPut, "/ssec/obj.txt", strings.NewReader("data"))
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	w := httptest.NewRecorder()
	rt.handleObjectOps(w, req, "ssec", "obj.txt")

	if w.Code == http.StatusOK {
		t.Fatalf("expected an error for a missing SSE-C key, got 200")
	}
	var errResp ErrorResponse
	if err := xml.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v (body: %s)", err, w.Body.String())
	}
	if errResp.Code != "MissingEncryptionKey" {
		t.Errorf("error code = %q, want MissingEncryptionKey (not the generic InvalidArgument)", errResp.Code)
	}

	// AES256 declared with a syntactically valid key but a wrong MD5.
	req = httptest.NewRequest(http.MethodPut, "/ssec/obj2.txt", strings.NewReader("data"))
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	req.Header.Set("x-amz-server-side-encryption-customer-key", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", "d29uZ21kNXZhbHVlZm9ydGVzdA==")
	w = httptest.NewRecorder()
	rt.handleObjectOps(w, req, "ssec", "obj2.txt")

	if w.Code == http.StatusOK {
		t.Fatalf("expected an error for a mismatched SSE-C key MD5, got 200")
	}
	errResp = ErrorResponse{}
	if err := xml.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v (body: %s)", err, w.Body.String())
	}
	if errResp.Code != "InvalidEncryptionKeyMD5" {
		t.Errorf("error code = %q, want InvalidEncryptionKeyMD5 (not the generic InvalidArgument)", errResp.Code)
	}
}

// TestPutBucketVersioningRejectsOversizedBody guards against unbounded
// xml.Decoder allocation. Several subresource PUT handlers used to decode
// r.Body directly with no size limit; a request whose payload hash is
// UNSIGNED-PAYLOAD (or any mode auth.HashPayload doesn't size-check) could
// send an arbitrarily large body and force the decoder to keep allocating.
// This sends a body padded well past the configured limit and asserts the
// handler rejects it as malformed (truncated) rather than decoding the whole
// thing.
func TestPutBucketVersioningRejectsOversizedBody(t *testing.T) {
	rt, eng := newOpsRouter(t)
	if err := eng.CreateBucket("limits"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// A well-formed VersioningConfiguration padded with a huge comment before
	// the closing tag, so it would parse successfully if read in full but
	// must be rejected once truncated at the body-size limit.
	padding := strings.Repeat("A", 3<<20) // 3MB, above the 2MB limit
	body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><!--` +
		padding + `--><Status>Enabled</Status></VersioningConfiguration>`

	req := httptest.NewRequest(http.MethodPut, "/limits?versioning", strings.NewReader(body))
	w := httptest.NewRecorder()
	rt.handleBucketOps(w, req, "limits")

	if w.Code == http.StatusOK {
		t.Fatalf("expected oversized body to be rejected, got 200 OK (body limit not enforced)")
	}

	var errResp ErrorResponse
	if err := xml.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v (body: %s)", err, w.Body.String())
	}
	if errResp.Code != "MalformedXML" {
		t.Errorf("error code = %q, want MalformedXML", errResp.Code)
	}
}
