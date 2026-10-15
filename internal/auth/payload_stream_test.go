package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func testSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// countingReader records how many bytes were read from the original body.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestLargeSignedPayloadIsVerifiedAsRead is the regression test for signed
// uploads being written to disk twice. A body over 2MiB was first spooled
// whole to a temp file so its SHA-256 could be checked, then copied again
// into the object: double the disk I/O for every signed upload, and a full
// extra copy of the object on disk while it happened. The hash is now checked
// as the body streams through, and a mismatch is reported as a read error
// before the end of the body — before storage can commit anything.
func TestLargeSignedPayloadIsVerifiedAsRead(t *testing.T) {
	creds := testCreds()
	verifier := NewSigV4Verifier(creds)
	body := bytes.Repeat([]byte("z"), 3<<20) // over the 2MiB in-memory threshold

	newReq := func(sent []byte) (*http.Request, *countingReader) {
		req, _ := http.NewRequest(http.MethodPut, "http://localhost:9000/b/k", nil)
		req.Host = "localhost:9000"
		signRequestWithBody(req, creds, "us-east-1", "s3", time.Now().UTC(), body, "")
		src := &countingReader{r: bytes.NewReader(sent)}
		req.Body = io.NopCloser(src)
		return req, src
	}

	t.Run("matching body streams through", func(t *testing.T) {
		req, src := newReq(body)
		if err := verifier.Verify(req); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if src.n > maxInMemoryPayload+1 {
			t.Fatalf("Verify consumed %d bytes up front; a large body must not be buffered whole", src.n)
		}
		if !IsDeferred(req.Body) {
			t.Error("a large signed body should be marked as verified-while-read")
		}
		got, err := io.ReadAll(req.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("read %d bytes, err %v; want the full body", len(got), err)
		}
	})

	t.Run("tampered body fails before its end", func(t *testing.T) {
		tampered := append([]byte(nil), body...)
		tampered[len(tampered)-1] = 'X'
		req, _ := newReq(tampered)
		if err := verifier.Verify(req); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		_, err := io.ReadAll(req.Body)
		var authErr *AuthError
		if !errors.As(err, &authErr) || authErr.Code != "XAmzContentSHA256Mismatch" {
			t.Fatalf("reading a tampered body: err = %v, want XAmzContentSHA256Mismatch", err)
		}
	})
}

// TestSmallSignedPayloadIsVerifiedUpFront: bodies that fit in memory keep
// being checked inside Verify, so handlers that stop reading early (an XML
// decoder can) never see an unverified small body.
func TestSmallSignedPayloadIsVerifiedUpFront(t *testing.T) {
	creds := testCreds()
	body := []byte("<Delete><Object><Key>a</Key></Object></Delete>")
	req, _ := http.NewRequest(http.MethodPut, "http://localhost:9000/b/k", nil)
	req.Host = "localhost:9000"
	signRequestWithBody(req, creds, "us-east-1", "s3", time.Now().UTC(), body, "")
	req.Body = io.NopCloser(bytes.NewReader(append([]byte("tampered"), body...)))

	err := NewSigV4Verifier(creds).Verify(req)
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != "XAmzContentSHA256Mismatch" {
		t.Fatalf("Verify of a tampered small body: err = %v, want XAmzContentSHA256Mismatch", err)
	}
	if IsDeferred(req.Body) {
		t.Error("a small body must not be deferred")
	}
}

// TestStreamingPayloadIsDecodedAsRead: aws-chunked bodies used to be decoded
// in full, and spooled to a temp file past 2MiB, before the handler saw a
// byte. They are now decoded chunk by chunk as the handler reads, holding one
// chunk in memory at a time.
func TestStreamingPayloadIsDecodedAsRead(t *testing.T) {
	creds := testCreds()
	var chunks [][]byte
	var want []byte
	for i := 0; i < 80; i++ { // 80 x 64KiB = 5MiB
		c := bytes.Repeat([]byte{byte('a' + i%26)}, 64<<10)
		chunks = append(chunks, c)
		want = append(want, c...)
	}
	req := buildStreamingRequest(t, creds, chunks, -1)
	full, _ := io.ReadAll(req.Body)
	src := &countingReader{r: bytes.NewReader(full)}
	req.Body = io.NopCloser(src)

	if err := NewSigV4Verifier(creds).Verify(req); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if src.n > 256<<10 {
		t.Fatalf("Verify consumed %d bytes up front; the stream must be decoded as it is read", src.n)
	}
	if req.ContentLength != int64(len(want)) {
		t.Errorf("ContentLength = %d, want the decoded length %d", req.ContentLength, len(want))
	}
	got, err := io.ReadAll(req.Body)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("decoded %d bytes, err %v; want %d bytes", len(got), err, len(want))
	}
}
