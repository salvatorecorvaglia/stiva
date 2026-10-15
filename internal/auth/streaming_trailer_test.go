package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	unsignedTrailerMode = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	signedTrailerMode   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
)

type trailerRequestOpts struct {
	declared         string   // x-amz-trailer header; "" omits it
	trailers         []string // "name:value" lines sent after the last chunk
	decodedLength    int      // x-amz-decoded-content-length; -1 omits it
	tamperTrailerSig bool
}

func crc32Trailer(data string) string {
	sum := crc32.ChecksumIEEE([]byte(data))
	b := []byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)}
	return "x-amz-checksum-crc32:" + base64.StdEncoding.EncodeToString(b)
}

// buildTrailerRequest encodes chunks in the aws-chunked trailer framing —
// the format recent AWS SDKs use by default to send an object checksum — and
// signs the request. mode selects unsigned chunks (what boto3 sends over
// HTTPS) or signed chunks plus a signed trailer.
func buildTrailerRequest(t *testing.T, creds *Credentials, mode string, chunks []string, o trailerRequestOpts) *http.Request {
	t.Helper()

	now := time.Now().UTC()
	datestamp, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	scope := datestamp + "/us-east-1/s3/aws4_request"
	host, path := "localhost:9000", "/testbucket/testkey"

	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": mode,
		"x-amz-date":           amzDate,
		"content-encoding":     "aws-chunked",
	}
	if o.declared != "" {
		headers["x-amz-trailer"] = o.declared
	}
	if o.decodedLength >= 0 {
		headers["x-amz-decoded-content-length"] = fmt.Sprint(o.decodedLength)
	}
	var names []string
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		fmt.Fprintf(&canonicalHeaders, "%s:%s\n", name, headers[name])
	}
	canonicalRequest := fmt.Sprintf("PUT\n%s\n\n%s\n%s\n%s", path, canonicalHeaders.String(), strings.Join(names, ";"), mode)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, scope, hashSHA256([]byte(canonicalRequest)))

	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), []byte(datestamp))
	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(kDate, []byte("us-east-1")), []byte("s3")), []byte("aws4_request"))
	seedSig := hex.EncodeToString(hmacSHA256Test(signingKey, []byte(stringToSign)))

	var body bytes.Buffer
	prevSig := seedSig
	signChunk := func(data []byte) string {
		sts := fmt.Sprintf("AWS4-HMAC-SHA256-PAYLOAD\n%s\n%s\n%s\n%s\n%s", amzDate, scope, prevSig, hashSHA256(nil), hashSHA256(data))
		prevSig = hex.EncodeToString(hmacSHA256Test(signingKey, []byte(sts)))
		return prevSig
	}
	for _, c := range append(chunks, "") {
		if mode == signedTrailerMode {
			fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(c), signChunk([]byte(c)))
		} else {
			fmt.Fprintf(&body, "%x\r\n", len(c))
		}
		if c != "" {
			body.WriteString(c + "\r\n")
		}
	}
	var canonicalTrailers strings.Builder
	for _, line := range o.trailers {
		body.WriteString(line + "\r\n")
		canonicalTrailers.WriteString(line + "\n")
	}
	if mode == signedTrailerMode {
		sts := fmt.Sprintf("AWS4-HMAC-SHA256-TRAILER\n%s\n%s\n%s\n%s", amzDate, scope, prevSig, hashSHA256([]byte(canonicalTrailers.String())))
		sig := hex.EncodeToString(hmacSHA256Test(signingKey, []byte(sts)))
		if o.tamperTrailerSig {
			sig = strings.Repeat("0", len(sig))
		}
		body.WriteString("x-amz-trailer-signature:" + sig + "\r\n")
	}
	body.WriteString("\r\n")

	req, err := http.NewRequest(http.MethodPut, "http://"+host+path, io.NopCloser(bytes.NewReader(body.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.ContentLength = int64(body.Len())
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope, strings.Join(names, ";"), seedSig))
	return req
}

func trailerCreds() *Credentials {
	return NewCredentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
}

func assertAuthCode(t *testing.T, err error, code string) {
	t.Helper()
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != code {
		t.Fatalf("err = %v, want AuthError %s", err, code)
	}
}

// TestStreamingTrailerModesDecode is the regression test for uploads from
// current AWS SDKs failing outright. They send the object checksum as an
// aws-chunked trailer by default (boto3 does over HTTPS), and Stiva compared
// the literal STREAMING-*-TRAILER marker against the hash of the raw body, so
// every such PUT was rejected with SignatureDoesNotMatch.
func TestStreamingTrailerModesDecode(t *testing.T) {
	for _, mode := range []string{unsignedTrailerMode, signedTrailerMode} {
		t.Run(mode, func(t *testing.T) {
			creds := trailerCreds()
			req := buildTrailerRequest(t, creds, mode, []string{"hello, ", "world!"}, trailerRequestOpts{
				declared:      "x-amz-checksum-crc32",
				trailers:      []string{crc32Trailer("hello, world!")},
				decodedLength: 13,
			})
			got, err := verifyAndRead(NewSigV4Verifier(creds), req)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if string(got) != "hello, world!" || req.ContentLength != 13 {
				t.Fatalf("decoded body %q (ContentLength %d), want %q", got, req.ContentLength, "hello, world!")
			}
		})
	}
}

// TestStreamingTrailerChecksumMismatch: the point of the trailer is the
// checksum, so a body that doesn't match it must be refused, not stored.
func TestStreamingTrailerChecksumMismatch(t *testing.T) {
	for _, mode := range []string{unsignedTrailerMode, signedTrailerMode} {
		t.Run(mode, func(t *testing.T) {
			creds := trailerCreds()
			req := buildTrailerRequest(t, creds, mode, []string{"hello, ", "world!"}, trailerRequestOpts{
				declared:      "x-amz-checksum-crc32",
				trailers:      []string{crc32Trailer("something else")},
				decodedLength: -1,
			})
			_, err := verifyAndRead(NewSigV4Verifier(creds), req)
			assertAuthCode(t, err, "BadDigest")
		})
	}
}

func TestStreamingSignedTrailerRejectsTamperedTrailerSignature(t *testing.T) {
	creds := trailerCreds()
	req := buildTrailerRequest(t, creds, signedTrailerMode, []string{"data"}, trailerRequestOpts{
		declared: "x-amz-checksum-crc32", trailers: []string{crc32Trailer("data")}, decodedLength: -1, tamperTrailerSig: true,
	})
	_, err := verifyAndRead(NewSigV4Verifier(creds), req)
	assertAuthCode(t, err, "SignatureDoesNotMatch")
}

// TestStreamingTrailerMustBeDeclared: trailers not announced in x-amz-trailer
// (which is covered by the request signature) are refused, as are
// algorithms S3 does not define.
func TestStreamingTrailerMustBeDeclared(t *testing.T) {
	creds := trailerCreds()
	cases := map[string]trailerRequestOpts{
		"undeclared trailer":  {trailers: []string{crc32Trailer("data")}, decodedLength: -1},
		"unknown algorithm":   {declared: "x-amz-checksum-md4", trailers: []string{"x-amz-checksum-md4:AAAA"}, decodedLength: -1},
		"declared but absent": {declared: "x-amz-checksum-crc32", decodedLength: -1},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			req := buildTrailerRequest(t, creds, unsignedTrailerMode, []string{"data"}, o)
			if _, err := verifyAndRead(NewSigV4Verifier(creds), req); err == nil {
				t.Fatal("a malformed trailer was accepted")
			}
		})
	}
}

// TestStreamingDecodedLengthMismatch: x-amz-decoded-content-length states the
// object size; a body that decodes to a different length was truncated or
// padded in transit.
func TestStreamingDecodedLengthMismatch(t *testing.T) {
	creds := trailerCreds()
	req := buildTrailerRequest(t, creds, unsignedTrailerMode, []string{"data"}, trailerRequestOpts{
		declared: "x-amz-checksum-crc32", trailers: []string{crc32Trailer("data")}, decodedLength: 99,
	})
	_, err := verifyAndRead(NewSigV4Verifier(creds), req)
	assertAuthCode(t, err, "IncompleteBody")
}
