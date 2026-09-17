package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// presignedRequest builds a presigned GET request for the given key, signed at
// the given time with the given validity window.
func presignedRequest(t *testing.T, v *SigV4Verifier, secretKey string, signedAt time.Time, expires int) *http.Request {
	t.Helper()

	datestamp := signedAt.Format("20060102")
	amzDate := signedAt.Format("20060102T150405Z")
	scope := datestamp + "/us-east-1/s3/aws4_request"

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", v.Creds().AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	if expires >= 0 {
		q.Set("X-Amz-Expires", strconv.Itoa(expires))
	}
	q.Set("X-Amz-SignedHeaders", "host")

	r := httptest.NewRequest(http.MethodGet, "/bucket/key.txt?"+q.Encode(), nil)
	r.Host = "localhost:9000"

	canonical := v.buildCanonicalRequestPresigned(r, []string{"host"})
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashSHA256([]byte(canonical))

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte("us-east-1"))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	sig := hmacSHA256(kSigning, []byte(stringToSign))

	q.Set("X-Amz-Signature", hexEncode(sig))
	r.URL.RawQuery = q.Encode()
	return r
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}

func newTestVerifier() *SigV4Verifier {
	return NewSigV4Verifier(NewCredentials("AKIATEST", "secretkey123"))
}

func TestPresignedValidRequestIsAccepted(t *testing.T) {
	v := newTestVerifier()
	r := presignedRequest(t, v, "secretkey123", time.Now().UTC(), 3600)

	if err := v.Verify(r); err != nil {
		t.Fatalf("a freshly signed presigned URL should verify, got: %v", err)
	}
}

// TestPresignedWithoutExpiresIsRejected is the regression test for the audit
// finding that expiry was only enforced when X-Amz-Expires was present, so a
// URL signed without it never expired.
func TestPresignedWithoutExpiresIsRejected(t *testing.T) {
	v := newTestVerifier()
	// Sign a URL well in the past that carries no expiry at all.
	r := presignedRequest(t, v, "secretkey123", time.Now().UTC().Add(-90*24*time.Hour), -1)

	err := v.Verify(r)
	if err == nil {
		t.Fatal("a presigned URL with no X-Amz-Expires must be rejected, not honoured forever")
	}

	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *AuthError, got %T", err)
	}
}

func TestPresignedExpiredIsRejected(t *testing.T) {
	v := newTestVerifier()
	// Signed two hours ago with a one-hour window.
	r := presignedRequest(t, v, "secretkey123", time.Now().UTC().Add(-2*time.Hour), 3600)

	err := v.Verify(r)
	if err == nil {
		t.Fatal("an expired presigned URL must be rejected")
	}
	if err.Error() != "Request has expired" {
		t.Errorf("unexpected error message: %v", err)
	}
}
