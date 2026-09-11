package auth

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	amzDateHeader        = "X-Amz-Date"
	unsignedPayload      = "UNSIGNED-PAYLOAD"
	streamingPayload     = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	chunkSignaturePrefix = "AWS4-HMAC-SHA256-PAYLOAD"
	aws4HMACSHA256Format = "AWS4-HMAC-SHA256\n%s\n%s\n%s"
	amzSignatureQuery    = "X-Amz-Signature"

	// maxClockSkew is the tolerated difference between the request timestamp
	// and server time, matching the AWS SigV4 window.
	maxClockSkew = 15 * time.Minute
	// maxPresignValidity is the longest lifetime a presigned URL may declare,
	// matching the S3 limit of seven days.
	maxPresignValidity = 7 * 24 * time.Hour
	// maxInMemoryPayload is the largest request body hashed entirely in memory.
	maxInMemoryPayload = 2 * 1024 * 1024
	// maxChunkSize bounds a single aws-chunked frame. The frame's declared
	// size is allocated up front to read it, and was checked only against
	// MaxPayloadSize (5GiB by default) — so one chunk header could drive a
	// 5GiB allocation before a single byte of data arrived. Real SDKs chunk
	// at 64KiB to 8MiB.
	maxChunkSize = 16 * 1024 * 1024
)

// MaxPayloadSize bounds the request body Stiva will buffer while verifying a
// signed payload hash. Bodies larger than this are rejected rather than spooled
// to disk, which previously let an authenticated client force unbounded
// temp-file writes. Zero disables the limit.
var MaxPayloadSize int64 = 5 * 1024 * 1024 * 1024 // 5GiB, the S3 single-PUT maximum

// TempDir is the directory where temporary request body files are stored.
var TempDir string

// AuthError represents a specific S3 authentication/signature error.
type AuthError struct {
	Code    string
	Message string
}

func (e *AuthError) Error() string {
	return e.Message
}

// maxSigningKeyCacheEntries bounds the derived-signing-key cache.
//
// The cache key is datestamp/region/service. All three come verbatim from the
// caller's credential scope, and the cache was an unbounded sync.Map that never
// evicted — so anyone who knew the access key (which is not a secret) could add
// one permanent entry per request, reached before the signature is even
// compared. Legitimate traffic needs a handful of entries at most: one region,
// one service, and two datestamps around a UTC midnight boundary.
//
// Like the bucket metadata cache in the storage layer, this drops everything at
// the bound rather than tracking recency — an entry is one HMAC chain to
// rebuild, and the bound is only a safety valve.
const maxSigningKeyCacheEntries = 64

// maxCredentialScopePart bounds a single component of the credential scope,
// which also bounds the size of a cache key built from it.
const maxCredentialScopePart = 64

// SigV4Verifier verifies AWS Signature Version 4 requests.
type SigV4Verifier struct {
	creds *Credentials

	keysMu      sync.Mutex
	signingKeys map[string][]byte
}

// NewSigV4Verifier creates a new verifier with the given credentials.
func NewSigV4Verifier(creds *Credentials) *SigV4Verifier {
	return &SigV4Verifier{
		creds:       creds,
		signingKeys: make(map[string][]byte, maxSigningKeyCacheEntries),
	}
}

// validateCredentialScope rejects a credential scope whose region or service
// could not have come from a real S3 client. Beyond being more correct than
// accepting anything, it keeps the signing-key cache key space small.
func validateCredentialScope(region, service string) error {
	if service != "s3" {
		return &AuthError{Code: "AuthorizationHeaderMalformed", Message: "Credential should be scoped to the service 's3'."}
	}
	if region == "" || len(region) > maxCredentialScopePart {
		return &AuthError{Code: "AuthorizationHeaderMalformed", Message: "Credential contains an invalid region."}
	}
	return nil
}

// Creds returns the credentials associated with this verifier.
func (v *SigV4Verifier) Creds() *Credentials {
	return v.creds
}

// parsedAuth holds the components parsed from the Authorization header.
type parsedAuth struct {
	Algorithm     string
	Credential    string
	AccessKey     string
	Date          string
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

// Verify checks the AWS SigV4 signature on an HTTP request.
// Returns nil if the signature is valid, or an error describing the failure.
func (v *SigV4Verifier) Verify(r *http.Request) error {
	// Check for query string authentication (presigned URLs)
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return v.verifyPresigned(r)
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return &AuthError{Code: "AccessDenied", Message: "missing Authorization header"}
	}

	parsed, err := parseAuthHeader(authHeader)
	if err != nil {
		return &AuthError{Code: "AccessDenied", Message: err.Error()}
	}

	if parsed.AccessKey != v.creds.AccessKey {
		return &AuthError{Code: "InvalidAccessKeyId", Message: "The Access Key Id you provided does not exist in our records."}
	}

	if err := validateCredentialScope(parsed.Region, parsed.Service); err != nil {
		return err
	}

	// Get the date for signing
	dateStr := r.Header.Get(amzDateHeader)
	if dateStr == "" {
		dateStr = r.Header.Get("Date")
	}
	if dateStr == "" {
		return &AuthError{Code: "AccessDenied", Message: "missing date header"}
	}

	// Parse the date
	t, err := time.Parse("20060102T150405Z", dateStr)
	if err != nil {
		// Try RFC2616 format
		t, err = time.Parse(time.RFC1123, dateStr)
		if err != nil {
			return &AuthError{Code: "AccessDenied", Message: "invalid date format"}
		}
	}

	// Check for date/time skew (standard is 15 minutes)
	if time.Now().UTC().Sub(t).Abs() > maxClockSkew {
		return &AuthError{Code: "RequestTimeTooSkewed", Message: "The difference between the request time and the current time is too large."}
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	isStreaming := payloadHash == streamingPayload

	datestamp := t.Format("20060102")

	// Reconstruct canonical request
	canonicalRequest := v.buildCanonicalRequest(r, parsed.SignedHeaders)

	// Build string to sign
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, parsed.Region, parsed.Service)
	stringToSign := fmt.Sprintf(aws4HMACSHA256Format,
		dateStr,
		scope,
		hashSHA256([]byte(canonicalRequest)),
	)

	// Calculate expected signature
	signingKey := v.deriveSigningKey(datestamp, parsed.Region, parsed.Service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(parsed.Signature)) {
		return &AuthError{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided."}
	}

	// Only now that the signature checks out do we touch the body. Hashing it
	// first meant an unauthenticated caller — the access key is not a secret —
	// could make the server spool up to MaxPayloadSize to disk per request and
	// only then be told the signature was wrong. The canonical request above
	// uses the *header* value, so verifying in this order changes nothing
	// about what is signed.
	if payloadHash != "" && payloadHash != unsignedPayload && !isStreaming {
		actualHash, err := HashPayload(r)
		if err != nil {
			return fmt.Errorf("failed to hash request payload: %w", err)
		}
		if actualHash != payloadHash {
			return &AuthError{
				Code:    "SignatureDoesNotMatch",
				Message: "The request signature we calculated does not match the signature you provided (payload hash mismatch).",
			}
		}
	}

	// STREAMING-AWS4-HMAC-SHA256-PAYLOAD carries the body as a sequence of
	// aws-chunked frames, each individually signed and chained from the
	// request's own signature above. The header signature alone says nothing
	// about the body, so it must be decoded and each chunk verified here —
	// otherwise the chunk framing (and any chunk whose declared signature
	// doesn't match its data) would be written to disk as if it were the
	// object's real content.
	if isStreaming {
		if err := decodeStreamingPayload(r, signingKey, dateStr, scope, expectedSig); err != nil {
			return err
		}
	}

	return nil
}

// verifyPresigned handles presigned URL verification.
func (v *SigV4Verifier) verifyPresigned(r *http.Request) error {
	q := r.URL.Query()

	credential := q.Get("X-Amz-Credential")
	parts := strings.Split(credential, "/")
	if len(parts) != 5 {
		return &AuthError{Code: "AccessDenied", Message: "invalid credential format"}
	}

	accessKey := parts[0]
	datestamp := parts[1]
	region := parts[2]
	service := parts[3]

	if accessKey != v.creds.AccessKey {
		return &AuthError{Code: "InvalidAccessKeyId", Message: "The Access Key Id you provided does not exist in our records."}
	}

	// The presigned credential scope is entirely caller-supplied, including the
	// datestamp, and feeds the signing-key cache key.
	if err := validateCredentialScope(region, service); err != nil {
		return err
	}
	if len(datestamp) > maxCredentialScopePart {
		return &AuthError{Code: "AccessDenied", Message: "invalid credential format"}
	}

	dateStr := q.Get(amzDateHeader)
	signedHeaders := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	signature := q.Get(amzSignatureQuery)

	// Expiry is mandatory. It used to be checked only when X-Amz-Expires was
	// present, so a URL signed without that parameter never expired at all.
	signedAt, err := time.Parse("20060102T150405Z", dateStr)
	if err != nil {
		return &AuthError{Code: "AccessDenied", Message: "invalid or missing X-Amz-Date"}
	}

	expires := q.Get("X-Amz-Expires")
	if expires == "" {
		return &AuthError{Code: "AccessDenied", Message: "X-Amz-Expires is required for presigned requests"}
	}

	dur, err := strconv.Atoi(expires)
	if err != nil || dur <= 0 {
		return &AuthError{Code: "AccessDenied", Message: "invalid expires value"}
	}
	if time.Duration(dur)*time.Second > maxPresignValidity {
		return &AuthError{
			Code:    "AccessDenied",
			Message: "X-Amz-Expires exceeds the maximum allowed value of 604800 seconds",
		}
	}

	now := time.Now().UTC()
	if now.After(signedAt.Add(time.Duration(dur) * time.Second)) {
		return &AuthError{Code: "AccessDenied", Message: "Request has expired"}
	}
	// Reject URLs dated far in the future, which would otherwise extend the
	// effective lifetime well beyond the stated expiry window.
	if signedAt.After(now.Add(maxClockSkew)) {
		return &AuthError{
			Code:    "RequestTimeTooSkewed",
			Message: "The difference between the request time and the current time is too large.",
		}
	}

	// Build canonical request for presigned URL
	// For presigned, the payload hash is unsignedPayload
	canonicalRequest := v.buildCanonicalRequestPresigned(r, signedHeaders)

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)
	stringToSign := fmt.Sprintf(aws4HMACSHA256Format,
		dateStr,
		scope,
		hashSHA256([]byte(canonicalRequest)),
	)

	signingKey := v.deriveSigningKey(datestamp, region, service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return &AuthError{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided."}
	}

	return nil
}

// parseAuthHeader parses an AWS SigV4 Authorization header.
// Format: AWS4-HMAC-SHA256 Credential=<accessKey>/<date>/<region>/<service>/aws4_request, SignedHeaders=<headers>, Signature=<sig>
func parseAuthHeader(header string) (*parsedAuth, error) {
	if !strings.HasPrefix(header, "AWS4-HMAC-SHA256 ") {
		return nil, fmt.Errorf("unsupported auth algorithm")
	}

	content := strings.TrimPrefix(header, "AWS4-HMAC-SHA256 ")
	parts := strings.Split(content, ", ")

	parsed := &parsedAuth{Algorithm: "AWS4-HMAC-SHA256"}

	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])

		switch key {
		case "Credential":
			parsed.Credential = value
			credParts := strings.Split(value, "/")
			if len(credParts) >= 5 {
				parsed.AccessKey = credParts[0]
				parsed.Date = credParts[1]
				parsed.Region = credParts[2]
				parsed.Service = credParts[3]
			}
		case "SignedHeaders":
			parsed.SignedHeaders = strings.Split(value, ";")
		case "Signature":
			parsed.Signature = value
		}
	}

	if parsed.AccessKey == "" || parsed.Signature == "" {
		return nil, fmt.Errorf("incomplete authorization header")
	}

	return parsed, nil
}

// buildCanonicalRequest constructs the canonical request string for SigV4.
func (v *SigV4Verifier) buildCanonicalRequest(r *http.Request, signedHeaders []string) string {
	// HTTP method
	method := r.Method

	// Canonical URI (URL-encoded path)
	canonicalURI := getCanonicalURI(r.URL)

	// Canonical query string
	canonicalQueryString := getCanonicalQueryString(r.URL.Query())

	// Canonical headers
	sort.Strings(signedHeaders)
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		value := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && value == "" {
			value = r.Host
		}
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(value)
		canonicalHeaders.WriteString("\n")
	}

	signedHeadersStr := strings.Join(signedHeaders, ";")

	// Payload hash
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeadersStr,
		payloadHash,
	)
}

// BuildCanonicalRequestPresigned constructs the canonical request for presigned URLs.
func (v *SigV4Verifier) BuildCanonicalRequestPresigned(r *http.Request, signedHeaders []string) string {
	return v.buildCanonicalRequestPresigned(r, signedHeaders)
}

// buildCanonicalRequestPresigned constructs the canonical request for presigned URLs.
func (v *SigV4Verifier) buildCanonicalRequestPresigned(r *http.Request, signedHeaders []string) string {
	method := r.Method
	canonicalURI := getCanonicalURI(r.URL)

	// For presigned URLs, exclude X-Amz-Signature from query string
	queryParams := r.URL.Query()
	queryParams.Del(amzSignatureQuery)
	canonicalQueryString := getCanonicalQueryString(queryParams)

	sort.Strings(signedHeaders)
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		value := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && value == "" {
			value = r.Host
		}
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(value)
		canonicalHeaders.WriteString("\n")
	}

	signedHeadersStr := strings.Join(signedHeaders, ";")

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeadersStr,
		unsignedPayload,
	)
}

// getCanonicalURI returns the URI-encoded path.
func getCanonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return path
}

func awsPercentEncode(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '~' || c == '.' {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

// GetCanonicalQueryString returns the sorted, URL-encoded query string.
func GetCanonicalQueryString(values url.Values) string {
	return getCanonicalQueryString(values)
}

// getCanonicalQueryString returns the sorted, URL-encoded query string.
func getCanonicalQueryString(values url.Values) string {
	if len(values) == 0 {
		return ""
	}

	var keys []string
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		vs := values[k]
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, awsPercentEncode(k)+"="+awsPercentEncode(v))
		}
	}

	return strings.Join(pairs, "&")
}

// DeriveSigningKey derives the SigV4 signing key.
func (v *SigV4Verifier) DeriveSigningKey(datestamp, region, service string) []byte {
	return v.deriveSigningKey(datestamp, region, service)
}

// deriveSigningKey derives the SigV4 signing key.
func (v *SigV4Verifier) deriveSigningKey(datestamp, region, service string) []byte {
	cacheKey := datestamp + "/" + region + "/" + service

	v.keysMu.Lock()
	if key, ok := v.signingKeys[cacheKey]; ok {
		v.keysMu.Unlock()
		return key
	}
	v.keysMu.Unlock()

	kDate := hmacSHA256([]byte("AWS4"+v.creds.SecretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	v.keysMu.Lock()
	if v.signingKeys == nil || len(v.signingKeys) >= maxSigningKeyCacheEntries {
		v.signingKeys = make(map[string][]byte, maxSigningKeyCacheEntries)
	}
	v.signingKeys[cacheKey] = kSigning
	v.keysMu.Unlock()

	return kSigning
}

// hmacSHA256 computes HMAC-SHA256.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

// HmacSHA256 computes HMAC-SHA256 and is exported for other packages.
func HmacSHA256(key, data []byte) []byte {
	return hmacSHA256(key, data)
}

// HashSHA256 computes SHA256 hash and returns hex string.
func HashSHA256(data []byte) string {
	return hashSHA256(data)
}

// hashSHA256 computes SHA256 hash and returns hex string.
func hashSHA256(data []byte) string {
	h := sha256.New()
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// HashPayload reads the request body, computes its SHA256, and returns the hash.
// It uses a temporary file to buffer large request bodies to prevent OOM errors.
func HashPayload(r *http.Request) (string, error) {
	if r.Body == nil {
		return hashSHA256([]byte("")), nil
	}

	// Reject oversized bodies up front. Spooling an unbounded body to disk to
	// hash it lets a single request fill the data volume.
	if MaxPayloadSize > 0 && r.ContentLength > MaxPayloadSize {
		return "", &AuthError{
			Code:    "EntityTooLarge",
			Message: "Your proposed upload exceeds the maximum allowed size.",
		}
	}

	lr := io.LimitReader(r.Body, maxInMemoryPayload+1)
	buf, err := io.ReadAll(lr)
	if err != nil {
		return "", err
	}

	if len(buf) <= maxInMemoryPayload {
		r.Body = io.NopCloser(bytes.NewReader(buf))
		return hashSHA256(buf), nil
	}

	tempDir := TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	tmpFile, err := os.CreateTemp(tempDir, "stiva-body-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for request body: %w", err)
	}
	tmpName := tmpFile.Name()

	if _, err := tmpFile.Write(buf); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return "", err
	}

	shaWriter := sha256.New()
	_, _ = shaWriter.Write(buf)

	// r.ContentLength is -1 for a chunked Transfer-Encoding request, so the
	// upfront check above never fires for one. Cap this copy independently of
	// Content-Length so such a request can't spool an unbounded body to disk.
	remaining := io.Reader(r.Body)
	if MaxPayloadSize > 0 {
		allowed := MaxPayloadSize - int64(len(buf))
		if allowed < 0 {
			allowed = 0
		}
		remaining = io.LimitReader(r.Body, allowed+1)
	}

	written, err := io.Copy(io.MultiWriter(tmpFile, shaWriter), remaining)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return "", err
	}
	if MaxPayloadSize > 0 && int64(len(buf))+written > MaxPayloadSize {
		tmpFile.Close()
		os.Remove(tmpName)
		return "", &AuthError{
			Code:    "EntityTooLarge",
			Message: "Your proposed upload exceeds the maximum allowed size.",
		}
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}

	fileRead, err := os.Open(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return "", err
	}

	r.Body = &tempFileReadCloser{
		File: fileRead,
		path: tmpName,
	}

	return hex.EncodeToString(shaWriter.Sum(nil)), nil
}

type tempFileReadCloser struct {
	*os.File
	path string
}

func (t *tempFileReadCloser) Close() error {
	if t.File == nil {
		return nil
	}
	err := t.File.Close()
	os.Remove(t.path)
	t.File = nil
	return err
}

// emptyPayloadHash is the SHA256 hash of an empty string, used by the
// aws-chunked chunk-signing string-to-sign as a stand-in for "hash of signed
// trailer headers" when the request carries none.
var emptyPayloadHash = hashSHA256(nil)

// decodeStreamingPayload replaces r.Body with the dechunked payload carried
// by an aws-chunked / STREAMING-AWS4-HMAC-SHA256-PAYLOAD request, verifying
// each chunk's signature against the chain seeded by the request's own
// Authorization signature. A chunk whose declared signature doesn't match is
// rejected outright rather than written to disk.
//
// Wire format per chunk: "<hex-size>;chunk-signature=<sig>\r\n<data>\r\n",
// terminated by a zero-size chunk. Trailer chunks (STREAMING-*-TRAILER, used
// for post-payload checksums) are not part of this format and are rejected
// by the caller before reaching here, since their payload hash header
// doesn't match streamingPayload.
func decodeStreamingPayload(r *http.Request, signingKey []byte, dateStr, scope, seedSignature string) error {
	if r.Body == nil {
		return nil
	}

	br := bufio.NewReader(r.Body)

	tempDir := TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}

	var mem bytes.Buffer
	var tmpFile *os.File
	var tmpName string
	cleanup := func() {
		if tmpFile != nil {
			tmpFile.Close()
			os.Remove(tmpName)
		}
	}
	spooled := int64(0)
	writeOut := func(p []byte) error {
		if MaxPayloadSize > 0 && spooled+int64(len(p)) > MaxPayloadSize {
			return &AuthError{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed size."}
		}
		spooled += int64(len(p))
		if tmpFile == nil && mem.Len()+len(p) > maxInMemoryPayload {
			f, err := os.CreateTemp(tempDir, "stiva-chunked-*")
			if err != nil {
				return fmt.Errorf("failed to create temp file for chunked payload: %w", err)
			}
			tmpFile = f
			tmpName = f.Name()
			if _, err := tmpFile.Write(mem.Bytes()); err != nil {
				return err
			}
			mem.Reset()
		}
		if tmpFile != nil {
			_, err := tmpFile.Write(p)
			return err
		}
		mem.Write(p)
		return nil
	}

	prevSignature := seedSignature
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			cleanup()
			return &AuthError{Code: "SignatureDoesNotMatch", Message: "malformed chunked payload: could not read chunk header"}
		}
		header = strings.TrimRight(header, "\r\n")

		sizeStr, declaredSig, ok := strings.Cut(header, ";chunk-signature=")
		if !ok {
			cleanup()
			return &AuthError{Code: "SignatureDoesNotMatch", Message: "malformed chunked payload: missing chunk-signature"}
		}
		chunkSize, err := strconv.ParseInt(sizeStr, 16, 64)
		if err != nil || chunkSize < 0 {
			cleanup()
			return &AuthError{Code: "SignatureDoesNotMatch", Message: "malformed chunked payload: invalid chunk size"}
		}
		if MaxPayloadSize > 0 && chunkSize > MaxPayloadSize {
			cleanup()
			return &AuthError{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed size."}
		}
		// Bound the allocation below, which happens before any data is read.
		if chunkSize > maxChunkSize {
			cleanup()
			return &AuthError{Code: "InvalidRequest", Message: "Chunk size exceeds the maximum allowed for an aws-chunked upload."}
		}

		chunkData := make([]byte, chunkSize)
		if chunkSize > 0 {
			if _, err := io.ReadFull(br, chunkData); err != nil {
				cleanup()
				return &AuthError{Code: "SignatureDoesNotMatch", Message: "malformed chunked payload: truncated chunk data"}
			}
		}
		trailer := make([]byte, 2)
		if _, err := io.ReadFull(br, trailer); err != nil || trailer[0] != '\r' || trailer[1] != '\n' {
			cleanup()
			return &AuthError{Code: "SignatureDoesNotMatch", Message: "malformed chunked payload: missing chunk terminator"}
		}

		stringToSign := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
			chunkSignaturePrefix,
			dateStr,
			scope,
			prevSignature,
			emptyPayloadHash,
			hashSHA256(chunkData),
		)
		expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
		if !hmac.Equal([]byte(expectedSig), []byte(declaredSig)) {
			cleanup()
			return &AuthError{Code: "SignatureDoesNotMatch", Message: "chunk signature does not match"}
		}
		prevSignature = expectedSig

		if chunkSize == 0 {
			break
		}
		if err := writeOut(chunkData); err != nil {
			cleanup()
			return err
		}
	}

	if tmpFile != nil {
		if err := tmpFile.Close(); err != nil {
			os.Remove(tmpName)
			return err
		}
		f, err := os.Open(tmpName)
		if err != nil {
			os.Remove(tmpName)
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			os.Remove(tmpName)
			return err
		}
		r.Body = &tempFileReadCloser{File: f, path: tmpName}
		r.ContentLength = fi.Size()
		return nil
	}

	data := mem.Bytes()
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	return nil
}

// PresignGetObject generates a presigned GET URL for S3 compatibility.
// extraParams carries additional query parameters to include in the signed
// URL, such as the "response-content-disposition" family of response-header
// overrides. It may be nil.
func PresignGetObject(accessKey, secretKey, region, bucket, key string, expires time.Duration, s3Endpoint string, extraParams url.Values) (string, error) {
	t := time.Now().UTC()
	datestamp := t.Format("20060102")
	amzDate := t.Format("20060102T150405Z")
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", datestamp, region)

	u, err := url.Parse(s3Endpoint)
	if err != nil {
		return "", err
	}
	host := u.Host

	query := url.Values{}
	for k, vs := range extraParams {
		for _, v := range vs {
			query.Add(k, v)
		}
	}
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", accessKey+"/"+credentialScope)
	query.Set(amzDateHeader, amzDate)
	query.Set("X-Amz-Expires", fmt.Sprintf("%.0f", expires.Seconds()))
	query.Set("X-Amz-SignedHeaders", "host")

	cleanKey := strings.TrimPrefix(key, "/")
	escapedPath := ""
	for _, part := range strings.Split(cleanKey, "/") {
		if escapedPath != "" {
			escapedPath += "/"
		}
		escapedPath += url.PathEscape(part)
	}

	canonicalPath := "/" + bucket + "/" + escapedPath

	// Sort query parameters for the canonical query string
	var keys []string
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonicalQueryParts []string
	for _, k := range keys {
		canonicalQueryParts = append(canonicalQueryParts, url.QueryEscape(k)+"="+url.QueryEscape(query.Get(k)))
	}
	canonicalQuery := strings.Join(canonicalQueryParts, "&")

	// Canonical headers
	canonicalHeaders := "host:" + host + "\n"
	signedHeaders := "host"

	// Canonical request
	canonicalRequest := fmt.Sprintf("GET\n%s\n%s\n%s\n%s\n"+unsignedPayload,
		canonicalPath,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
	)

	// String to sign
	stringToSign := fmt.Sprintf(aws4HMACSHA256Format,
		amzDate,
		credentialScope,
		hashSHA256([]byte(canonicalRequest)),
	)

	// Signing key derivation
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	// Calculate signature
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	// Append signature to query
	query.Set(amzSignatureQuery, signature)

	endpoint := strings.TrimSuffix(s3Endpoint, "/")

	return endpoint + canonicalPath + "?" + query.Encode(), nil
}

// SignRequest signs an HTTP request with AWS Signature Version 4.
func SignRequest(req *http.Request, accessKey, secretKey, region, service string) {
	t := time.Now().UTC()
	dateBasic := t.Format("20060102T150405Z")
	dateDay := t.Format("20060102")

	req.Header.Set("X-Amz-Date", dateBasic)

	// Set Host header
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if host == "" {
		host = "localhost"
	}
	req.Header.Set("Host", host)

	// For S3 compatibility with large stream requests, we sign with UNSIGNED-PAYLOAD.
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	hashedPayload := "UNSIGNED-PAYLOAD"

	// Canonical headers to sign
	headers := []string{"host", "x-amz-date", "x-amz-content-sha256"}
	if req.Header.Get("Content-Type") != "" {
		headers = append(headers, "content-type")
	}
	sort.Strings(headers)

	var canonicalHeaders strings.Builder
	for _, h := range headers {
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(req.Header.Get(h)))
		canonicalHeaders.WriteString("\n")
	}

	signedHeaders := strings.Join(headers, ";")

	// Escaping path correctly
	escapedPath := req.URL.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"
	}

	canonicalQuery := req.URL.Query().Encode()

	// Canonical Request
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method,
		escapedPath,
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		hashedPayload,
	)

	hReq := sha256.Sum256([]byte(canonicalRequest))
	hashedCanonicalRequest := hex.EncodeToString(hReq[:])

	// Credential Scope
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateDay, region, service)

	// String to Sign
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		dateBasic,
		credentialScope,
		hashedCanonicalRequest,
	)

	// Deriving Key
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateDay))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	// Signature
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	// Auth Header
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey,
		credentialScope,
		signedHeaders,
		signature,
	)

	req.Header.Set("Authorization", authHeader)
}
